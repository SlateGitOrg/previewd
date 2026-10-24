package intake

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/archit2501/previewd/api/v1alpha1"
)

const sha1 = "4f2a9c1e0b7d3a5f6c8e9d0a1b2c3d4e5f6a7b8c"
const sha2 = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newSink(t *testing.T, objs ...client.Object) (*Sink, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	rules, err := ParseRules([]string{"acme/*=default", "other/site=static-site"}, "default")
	if err != nil {
		t.Fatal(err)
	}
	return &Sink{Client: c, Rules: rules, Log: logr.Discard()}, c
}

func event(action string, state v1alpha1.PRState, sha string, at time.Time) Event {
	return Event{
		DeliveryID: "d-" + action,
		Action:     action,
		Repo:       v1alpha1.RepositoryRef{Owner: "acme", Name: "shop"},
		PR: v1alpha1.PullRequestRef{
			Number: 42, HeadSHA: sha, HeadRef: "feature/x", BaseRef: "main",
			Author: "octocat", Title: "Add x", Labels: []string{"preview", "area/ui"},
		},
		State:     state,
		UpdatedAt: at,
	}
}

func get(t *testing.T, c client.Client, name string) *v1alpha1.PreviewEnvironment {
	t.Helper()
	var pe v1alpha1.PreviewEnvironment
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &pe); err != nil {
		t.Fatal(err)
	}
	return &pe
}

func TestLifecycleOfEvents(t *testing.T) {
	ctx := context.Background()
	s, c := newSink(t)

	res, err := s.Apply(ctx, event("opened", v1alpha1.PRStateOpen, sha1, t0))
	if err != nil || res.Outcome != OutcomeCreated || res.Instance != "acme-shop-pr-42" {
		t.Fatalf("opened: %+v %v", res, err)
	}
	pe := get(t, c, "acme-shop-pr-42")
	if pe.Spec.Template != "default" || pe.Spec.PullRequest.HeadSHA != sha1 || pe.Spec.State != v1alpha1.PRStateOpen {
		t.Fatalf("spec %+v", pe.Spec)
	}
	if strings.Join(pe.Spec.PullRequest.Labels, ",") != "area/ui,preview" {
		t.Fatalf("labels not sorted: %v", pe.Spec.PullRequest.Labels)
	}

	// Idempotent redelivery.
	res, err = s.Apply(ctx, event("opened", v1alpha1.PRStateOpen, sha1, t0))
	if err != nil || res.Outcome != OutcomeUnchanged {
		t.Fatalf("redelivery: %+v %v", res, err)
	}

	// New push.
	res, err = s.Apply(ctx, event("synchronize", v1alpha1.PRStateOpen, sha2, t0.Add(time.Minute)))
	if err != nil || res.Outcome != OutcomeUpdated {
		t.Fatalf("synchronize: %+v %v", res, err)
	}

	// A delayed delivery of an older event must not roll the SHA back.
	res, err = s.Apply(ctx, event("synchronize", v1alpha1.PRStateOpen, sha1, t0.Add(30*time.Second)))
	if err != nil || res.Outcome != OutcomeStale {
		t.Fatalf("stale: %+v %v", res, err)
	}
	if get(t, c, "acme-shop-pr-42").Spec.PullRequest.HeadSHA != sha2 {
		t.Fatal("stale event rolled back the head SHA")
	}

	// Operator TTL override survives later events.
	pe = get(t, c, "acme-shop-pr-42")
	pe.Spec.TTL = &metav1.Duration{Duration: time.Hour}
	if err := c.Update(ctx, pe); err != nil {
		t.Fatal(err)
	}
	res, err = s.Apply(ctx, event("closed", v1alpha1.PRStateClosed, sha2, t0.Add(2*time.Minute)))
	if err != nil || res.Outcome != OutcomeUpdated {
		t.Fatalf("closed: %+v %v", res, err)
	}
	pe = get(t, c, "acme-shop-pr-42")
	if pe.Spec.State != v1alpha1.PRStateClosed || pe.Spec.TTL == nil || pe.Spec.TTL.Duration != time.Hour {
		t.Fatalf("spec after close %+v", pe.Spec)
	}
}

func TestClosedWithoutPreviewIsIgnored(t *testing.T) {
	s, _ := newSink(t)
	res, err := s.Apply(context.Background(), event("closed", v1alpha1.PRStateClosed, sha1, t0))
	if err != nil || res.Outcome != OutcomeIgnored {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRepositoryRules(t *testing.T) {
	s, c := newSink(t)
	ev := event("opened", v1alpha1.PRStateOpen, sha1, t0)
	ev.Repo = v1alpha1.RepositoryRef{Owner: "evil", Name: "repo"}
	res, err := s.Apply(context.Background(), ev)
	if err != nil || res.Outcome != OutcomeIgnored || res.Reason != "repository not allowed" {
		t.Fatalf("%+v %v", res, err)
	}
	ev.Repo = v1alpha1.RepositoryRef{Owner: "Other", Name: "Site"}
	res, err = s.Apply(context.Background(), ev)
	if err != nil || res.Outcome != OutcomeCreated {
		t.Fatalf("%+v %v", res, err)
	}
	if get(t, c, res.Instance).Spec.Template != "static-site" {
		t.Fatal("rule template not used")
	}
}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules([]string{" acme/shop ", "", "*/*=x"}, "default")
	if err != nil || len(rules) != 2 || rules[0].Template != "default" || rules[1].Template != "x" {
		t.Fatalf("%+v %v", rules, err)
	}
	for _, bad := range [][]string{nil, {"noslash"}, {"[/x"}} {
		if _, err := ParseRules(bad, "d"); err == nil {
			t.Fatalf("expected error for %v", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	good := event("opened", v1alpha1.PRStateOpen, sha1, t0)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	long := good
	long.PR.HeadSHA = strings.Repeat("a", 64)
	if err := long.Validate(); err != nil {
		t.Fatal("sha256 commit ids are valid", err)
	}
	bad := Event{PR: v1alpha1.PullRequestRef{HeadSHA: "XYZ"}, State: "merged"}
	err := bad.Validate()
	for _, want := range []string{"owner", "positive", "hex", "unknown state", "updated_at"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	upper := good
	upper.PR.HeadSHA = strings.ToUpper(sha1)
	if upper.Validate() == nil {
		t.Fatal("uppercase sha accepted")
	}
	s, _ := newSink(t)
	if _, err := s.Apply(context.Background(), bad); err == nil {
		t.Fatal("invalid event applied")
	}
}

func TestDeletingPreviewIsRetryable(t *testing.T) {
	now := metav1.NewTime(t0)
	existing := &v1alpha1.PreviewEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-shop-pr-42", DeletionTimestamp: &now, Finalizers: []string{v1alpha1.Finalizer}},
		Spec:       specFrom(event("opened", v1alpha1.PRStateOpen, sha1, t0), "default"),
	}
	s, _ := newSink(t, existing)
	_, err := s.Apply(context.Background(), event("reopened", v1alpha1.PRStateOpen, sha2, t0.Add(time.Hour)))
	if !errors.Is(err, ErrDeleting) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateRaceRetriesAsUpdate(t *testing.T) {
	sch := scheme(t)
	created := false
	var c client.Client
	c = fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if !created {
				// Simulate another replica winning the race: the object
				// appears, and our create fails with AlreadyExists.
				created = true
				other := obj.DeepCopyObject().(*v1alpha1.PreviewEnvironment)
				other.Spec.PullRequest.HeadSHA = sha1
				if err := cl.Create(ctx, other, opts...); err != nil {
					return err
				}
				return cl.Create(ctx, obj, opts...)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()
	rules, _ := ParseRules([]string{"acme/*"}, "default")
	s := &Sink{Client: c, Rules: rules, Log: logr.Discard()}
	res, err := s.Apply(context.Background(), event("synchronize", v1alpha1.PRStateOpen, sha2, t0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("outcome %s", res.Outcome)
	}
	if get(t, c, res.Instance).Spec.PullRequest.HeadSHA != sha2 {
		t.Fatal("retry did not apply the event")
	}
}

func TestClientErrorsPropagate(t *testing.T) {
	boom := errors.New("apiserver down")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	rules, _ := ParseRules([]string{"acme/*"}, "default")
	s := &Sink{Client: c, Rules: rules, Log: logr.Discard()}
	if _, err := s.Apply(context.Background(), event("opened", v1alpha1.PRStateOpen, sha1, t0)); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestSpecEqual(t *testing.T) {
	a := specFrom(event("opened", v1alpha1.PRStateOpen, sha1, t0), "default")
	b := specFrom(event("opened", v1alpha1.PRStateOpen, sha1, t0), "default")
	if !specEqual(a, b) {
		t.Fatal("equal specs differ")
	}
	b.PullRequest.Labels = []string{"area/ui", "other"}
	if specEqual(a, b) {
		t.Fatal("label change not detected")
	}
	c := a
	c.TTL = &metav1.Duration{Duration: time.Minute}
	if specEqual(a, c) {
		t.Fatal("ttl change not detected")
	}
	d := a
	d.PullRequest.Draft = true
	if specEqual(a, d) {
		t.Fatal("draft change not detected")
	}
}
