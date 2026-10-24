package scm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/lifecycle"
)

const sha = "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"

func status(state lifecycle.ReportState) Status {
	return Status{Repo: v1alpha1.RepositoryRef{Owner: "acme", Name: "shop"}, SHA: sha, State: state, URL: "https://pr-shop-42.preview.example.test"}
}

func TestGitHubPostsStatus(t *testing.T) {
	var got statusRequest
	var path, auth, version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, version = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-GitHub-Api-Version")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	g := &GitHub{BaseURL: srv.URL + "/", Token: "tkn", Context: "previews/acme"}
	if err := g.Report(context.Background(), status(lifecycle.ReportReady)); err != nil {
		t.Fatal(err)
	}
	if path != "/repos/acme/shop/statuses/"+sha || auth != "Bearer tkn" || version != "2022-11-28" {
		t.Fatalf("path=%s auth=%s version=%s", path, auth, version)
	}
	if got.State != "success" || got.Context != "previews/acme" || got.TargetURL == "" {
		t.Fatalf("%+v", got)
	}
}

func TestStateMapping(t *testing.T) {
	want := map[lifecycle.ReportState]string{
		lifecycle.ReportPending: "pending", lifecycle.ReportReady: "success",
		lifecycle.ReportFailed: "failure", lifecycle.ReportExpired: "error",
		lifecycle.ReportInactive: "success",
	}
	for in, w := range want {
		if got, desc := githubState(in); got != w || desc == "" {
			t.Errorf("%s -> %s (%q)", in, got, desc)
		}
	}
}

func TestDescriptionIsTruncatedAndDefaultsApply(t *testing.T) {
	var got statusRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	s := status(lifecycle.ReportFailed)
	s.Description = strings.Repeat("x", 200)
	if err := (&GitHub{BaseURL: srv.URL}).Report(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(got.Description) != 140 || !strings.HasSuffix(got.Description, "...") || got.Context != "previewd" {
		t.Fatalf("%d %q %q", len(got.Description), got.Description[130:], got.Context)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	var slept []time.Duration
	g := &GitHub{BaseURL: srv.URL, Backoff: time.Second, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}
	if err := g.Report(context.Background(), status(lifecycle.ReportPending)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(slept) != 2 || slept[0] != 7*time.Second || slept[1] != 2*time.Second {
		t.Fatalf("calls=%d slept=%v", calls.Load(), slept)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"No commit found for SHA"}`))
	}))
	defer srv.Close()
	err := (&GitHub{BaseURL: srv.URL}).Report(context.Background(), status(lifecycle.ReportPending))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 || apiErr.Message != "No commit found for SHA" || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestRateLimitedForbiddenIsTransientAndExhausts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()
	g := &GitHub{BaseURL: srv.URL, MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }}
	err := g.Report(context.Background(), status(lifecycle.ReportPending))
	if err == nil || !strings.Contains(err.Error(), "transient HTTP 403") || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &GitHub{BaseURL: srv.URL, Backoff: time.Hour}
	if err := g.Report(ctx, status(lifecycle.ReportPending)); err == nil {
		t.Fatal("expected error")
	}
	// The real sleeper honours cancellation too.
	if err := g.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := g.sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkErrorAndBadURL(t *testing.T) {
	g := &GitHub{BaseURL: "http://127.0.0.1:1", MaxAttempts: 1}
	if err := g.Report(context.Background(), status(lifecycle.ReportPending)); err == nil {
		t.Fatal("expected network error")
	}
	g = &GitHub{BaseURL: "http://bad host"}
	var apiErr *APIError
	if err := g.Report(context.Background(), status(lifecycle.ReportPending)); !errors.As(err, &apiErr) {
		t.Fatalf("expected request construction error, got %v", err)
	}
	if (&GitHub{}).backoff(3) != 2*time.Second {
		t.Fatal("default backoff")
	}
}

func TestRecorderAndLogReporter(t *testing.T) {
	r := &Recorder{}
	boom := errors.New("boom")
	r.FailNext(1, boom)
	if err := r.Report(context.Background(), status(lifecycle.ReportPending)); !errors.Is(err, boom) {
		t.Fatal("expected injected failure")
	}
	if err := r.Report(context.Background(), status(lifecycle.ReportReady)); err != nil {
		t.Fatal(err)
	}
	if s := r.Statuses(); len(s) != 1 || s[0].State != lifecycle.ReportReady {
		t.Fatalf("%+v", s)
	}
	if err := (LogReporter{Log: logr.Discard()}).Report(context.Background(), status(lifecycle.ReportReady)); err != nil {
		t.Fatal(err)
	}
}
