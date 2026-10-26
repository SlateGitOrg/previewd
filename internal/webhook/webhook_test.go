package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/intake"
	"github.com/SlateGitOrg/previewd/internal/metrics"
)

var secret = []byte("s3cr3t-for-tests")

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	sig := Sign(secret, body)
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != 7+64 {
		t.Fatalf("sig %s", sig)
	}
	if err := Verify([][]byte{secret}, body, sig); err != nil {
		t.Fatal(err)
	}
	// Rotation: old and new secrets both accepted.
	if err := Verify([][]byte{[]byte("new"), secret}, body, sig); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		secrets [][]byte
		body    []byte
		header  string
		want    error
	}{
		"no secrets":     {nil, body, sig, ErrNoSecrets},
		"missing header": {[][]byte{secret}, body, "", ErrMissingSignature},
		"sha1 header":    {[][]byte{secret}, body, "sha1=abc", ErrMalformedSignature},
		"not hex":        {[][]byte{secret}, body, "sha256=zz", ErrMalformedSignature},
		"short":          {[][]byte{secret}, body, "sha256=abcd", ErrMalformedSignature},
		"wrong secret":   {[][]byte{[]byte("other")}, body, sig, ErrSignatureMismatch},
		"tampered body":  {[][]byte{secret}, []byte(`{"hello":"w0rld"}`), sig, ErrSignatureMismatch},
	}
	for name, tc := range cases {
		if err := Verify(tc.secrets, tc.body, tc.header); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v want %v", name, err, tc.want)
		}
	}
}

// TestKnownVector pins the HMAC to an independently computed value
// (python3 -c 'import hmac,hashlib;print(hmac.new(b"It\x27s a Secret to Everybody",b"Hello, World!",hashlib.sha256).hexdigest())'),
// which is also the example in GitHub's webhook documentation.
func TestKnownVector(t *testing.T) {
	got := Sign([]byte("It's a Secret to Everybody"), []byte("Hello, World!"))
	want := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func FuzzVerifyNeverPanics(f *testing.F) {
	f.Add([]byte("body"), "sha256=00")
	f.Add([]byte(""), "")
	f.Fuzz(func(t *testing.T, body []byte, header string) {
		err := Verify([][]byte{secret}, body, header)
		if err == nil && header != Sign(secret, body) && !strings.EqualFold(header, Sign(secret, body)) {
			t.Fatalf("accepted forged header %q", header)
		}
	})
}

func TestDeliveryCache(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewDeliveryCache(2, time.Minute, func() time.Time { return now })
	if c.Seen("a") {
		t.Fatal("empty cache saw a")
	}
	c.Record("")
	if c.Seen("") || c.Len() != 0 {
		t.Fatal("empty ids must be ignored")
	}
	c.Record("a")
	c.Record("b")
	c.Record("a") // refresh a
	c.Record("c") // evicts b, the least recent
	if !c.Seen("a") || c.Seen("b") || !c.Seen("c") || c.Len() != 2 {
		t.Fatal("LRU eviction wrong")
	}
	now = now.Add(2 * time.Minute)
	if c.Seen("a") || c.Len() != 1 {
		t.Fatal("TTL expiry wrong")
	}
	d := NewDeliveryCache(0, time.Minute, nil)
	d.Record("x")
	d.Record("y")
	if d.Len() != 1 {
		t.Fatal("capacity floor")
	}
}

func TestDeliveryCacheConcurrent(t *testing.T) {
	c := NewDeliveryCache(100, time.Hour, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 500 {
				id := string(rune('a'+i)) + string(rune(j))
				c.Record(id)
				c.Seen(id)
			}
		}()
	}
	wg.Wait()
	if c.Len() != 100 {
		t.Fatalf("len %d", c.Len())
	}
}

func TestParseFixtures(t *testing.T) {
	ev, err := ParsePullRequest(fixture(t, "01-opened.json"), "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Repo.FullName() != "acme/shop" || ev.PR.Number != 42 || ev.State != v1alpha1.PRStateOpen ||
		ev.PR.HeadRef != "feature/checkout-v2" || ev.PR.Author != "octocat" || len(ev.PR.Labels) != 1 ||
		ev.UpdatedAt != time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) {
		t.Fatalf("%+v", ev)
	}
	ev, err = ParsePullRequest(fixture(t, "06-closed.json"), "d6", false)
	if err != nil || ev.State != v1alpha1.PRStateClosed {
		t.Fatalf("%+v %v", ev, err)
	}
	if _, err := ParsePullRequest(fixture(t, "04-assigned.json"), "d4", false); !IsIrrelevant(err) {
		t.Fatalf("assigned: %v", err)
	}
	if _, err := ParsePullRequest(fixture(t, "05-fork-opened.json"), "d5", false); !IsIrrelevant(err) || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("fork: %v", err)
	}
	if ev, err := ParsePullRequest(fixture(t, "05-fork-opened.json"), "d5", true); err != nil || ev.PR.Number != 43 {
		t.Fatalf("fork allowed: %+v %v", ev, err)
	}
}

func TestParseEdgeCases(t *testing.T) {
	if _, err := ParsePullRequest([]byte("{"), "", false); err == nil || IsIrrelevant(err) {
		t.Fatalf("broken json: %v", err)
	}
	// Repository given only by full_name; number only at top level.
	body := `{"action":"opened","number":7,"pull_request":{"state":"open","updated_at":"2026-10-01T00:00:00Z",
	  "head":{"sha":"4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c","ref":"x","repo":{"full_name":"ACME/api"}},"base":{"ref":"main"}},
	  "repository":{"full_name":"acme/api"}}`
	ev, err := ParsePullRequest([]byte(body), "", false)
	if err != nil || ev.Repo.Name != "api" || ev.PR.Number != 7 {
		t.Fatalf("%+v %v", ev, err)
	}
	invalid := `{"action":"opened","pull_request":{"head":{"sha":"nothex","repo":{"full_name":"a/b"}}},"repository":{"full_name":"a/b"}}`
	if _, err := ParsePullRequest([]byte(invalid), "", false); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("got %v", err)
	}
}

// GitHub sends head.repo = null once the fork behind a PR is deleted. With
// forks disabled that must not count as a same-repository PR (THREAT_MODEL
// T9): the guard fails closed.
func TestForkGuardFailsClosedOnUnknownHeadRepo(t *testing.T) {
	const sha = "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"
	payload := func(head string) []byte {
		return []byte(`{"action":"opened","number":7,"pull_request":{"number":7,"state":"open","updated_at":"2026-10-01T00:00:00Z",
		  "head":{"sha":"` + sha + `","ref":"x"` + head + `},"base":{"ref":"main"}},
		  "repository":{"name":"shop","full_name":"acme/shop","owner":{"login":"acme"}}}`)
	}
	for name, head := range map[string]string{
		"null repo":      `,"repo":null`,
		"missing repo":   ``,
		"empty fullname": `,"repo":{"full_name":""}`,
	} {
		if ev, err := ParsePullRequest(payload(head), "d", false); !IsIrrelevant(err) || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%s, forks disabled: event %+v err %v; want ignored", name, ev, err)
		}
		if ev, err := ParsePullRequest(payload(head), "d", true); err != nil || ev.PR.Number != 7 {
			t.Errorf("%s, forks allowed: event %+v err %v", name, ev, err)
		}
	}
	if ev, err := ParsePullRequest(payload(`,"repo":{"full_name":"acme/shop"}`), "d", false); err != nil || ev.PR.Number != 7 {
		t.Errorf("same-repo PR: %+v %v", ev, err)
	}
}

type fakeSink struct {
	mu     sync.Mutex
	events []intake.Event
	res    intake.Result
	err    error
}

func (f *fakeSink) Apply(_ context.Context, ev intake.Event) (intake.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.res, f.err
}

func do(t *testing.T, h http.Handler, method, event, delivery string, body []byte, sig string) (*httptest.ResponseRecorder, Response) {
	t.Helper()
	req := httptest.NewRequest(method, "/webhook", bytes.NewReader(body))
	if event != "" {
		req.Header.Set(EventHeader, event)
	}
	if delivery != "" {
		req.Header.Set(DeliveryHeader, delivery)
	}
	if sig != "" {
		req.Header.Set(SignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestHandlerStatusCodes(t *testing.T) {
	opened := fixture(t, "01-opened.json")
	ping := fixture(t, "00-ping.json")
	sink := &fakeSink{res: intake.Result{Outcome: intake.OutcomeCreated, Instance: "acme-shop-pr-42"}}
	h := &Handler{Secrets: [][]byte{secret}, Sink: sink, Deliveries: NewDeliveryCache(10, time.Hour, nil), Log: logr.Discard()}

	before := testutil.ToFloat64(metrics.WebhookRequests.WithLabelValues("pull_request", "unauthorized"))
	cases := []struct {
		name, method, event, delivery string
		body                          []byte
		sig                           string
		code                          int
		status                        string
	}{
		{"wrong method", http.MethodGet, "pull_request", "", nil, "", 405, "method not allowed"},
		{"unsigned", http.MethodPost, "pull_request", "x", opened, "", 401, "invalid signature"},
		{"forged", http.MethodPost, "pull_request", "x", opened, Sign([]byte("wrong"), opened), 401, "invalid signature"},
		{"ping", http.MethodPost, "ping", "p1", ping, Sign(secret, ping), 200, "pong"},
		{"no event header", http.MethodPost, "", "x", opened, Sign(secret, opened), 400, "missing X-GitHub-Event header"},
		{"other event", http.MethodPost, "push", "x", opened, Sign(secret, opened), 202, "ignored"},
		{"malformed", http.MethodPost, "pull_request", "m", []byte("{}"), Sign(secret, []byte("{}")), 202, "ignored"},
		{"broken json", http.MethodPost, "pull_request", "b", []byte("{"), Sign(secret, []byte("{")), 400, "bad request"},
		{"accepted", http.MethodPost, "pull_request", "d1", opened, Sign(secret, opened), 202, "created"},
		{"duplicate", http.MethodPost, "pull_request", "d1", opened, Sign(secret, opened), 200, "duplicate"},
	}
	for _, tc := range cases {
		rec, resp := do(t, h, tc.method, tc.event, tc.delivery, tc.body, tc.sig)
		if rec.Code != tc.code || resp.Status != tc.status {
			t.Errorf("%s: got %d %q, want %d %q", tc.name, rec.Code, resp.Status, tc.code, tc.status)
		}
	}
	if len(sink.events) != 1 {
		t.Fatalf("sink saw %d events, want exactly 1", len(sink.events))
	}
	after := testutil.ToFloat64(metrics.WebhookRequests.WithLabelValues("pull_request", "unauthorized"))
	if after-before != 2 {
		t.Fatalf("unauthorized metric moved by %v", after-before)
	}
}

func TestHandlerSinkErrors(t *testing.T) {
	opened := fixture(t, "01-opened.json")
	sink := &fakeSink{err: errors.New("apiserver unavailable")}
	cache := NewDeliveryCache(10, time.Hour, nil)
	h := &Handler{Secrets: [][]byte{secret}, Sink: sink, Deliveries: cache, Log: logr.Discard()}
	rec, _ := do(t, h, http.MethodPost, "pull_request", "e1", opened, Sign(secret, opened))
	if rec.Code != 500 {
		t.Fatalf("code %d", rec.Code)
	}
	if cache.Seen("e1") {
		t.Fatal("failed delivery must stay retryable")
	}
	sink.err = intake.ErrDeleting
	rec, _ = do(t, h, http.MethodPost, "pull_request", "e1", opened, Sign(secret, opened))
	if rec.Code != 503 {
		t.Fatalf("code %d", rec.Code)
	}
	sink.err = nil
	sink.res = intake.Result{Outcome: intake.OutcomeUpdated}
	rec, resp := do(t, h, http.MethodPost, "pull_request", "e1", opened, Sign(secret, opened))
	if rec.Code != 202 || resp.Status != "updated" {
		t.Fatalf("redelivery after failure: %d %+v", rec.Code, resp)
	}
}

func TestHandlerBodyLimit(t *testing.T) {
	h := &Handler{Secrets: [][]byte{secret}, Sink: &fakeSink{}, MaxBodyBytes: 16, Log: logr.Discard()}
	big := bytes.Repeat([]byte("a"), 64)
	rec, _ := do(t, h, http.MethodPost, "pull_request", "", big, Sign(secret, big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rec.Code)
	}
	if (&Handler{}).maxBody() != DefaultMaxBodyBytes {
		t.Fatal("default body limit")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestHandlerUnreadableBody(t *testing.T) {
	h := &Handler{Secrets: [][]byte{secret}, Sink: &fakeSink{}, Log: logr.Discard()}
	req := httptest.NewRequest(http.MethodPost, "/webhook", errReader{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d", rec.Code)
	}
}
