package gc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/archit2501/previewd/api/v1alpha1"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ns(name, instance string, age time.Duration, managed bool) *corev1.Namespace {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		CreationTimestamp: metav1.NewTime(now.Add(-age)),
		Labels:            map[string]string{v1alpha1.LabelInstance: instance},
	}}
	if managed {
		n.Labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByValue
	}
	return n
}

func TestSelectOrphans(t *testing.T) {
	terminating := ns("pr-term-1", "gone-1", time.Hour, true)
	dt := metav1.NewTime(now)
	terminating.DeletionTimestamp = &dt
	list := []corev1.Namespace{
		*ns("pr-live-1", "live-pr-1", time.Hour, true),
		*ns("pr-orphan-2", "gone-pr-2", time.Hour, true),
		*ns("pr-young-3", "gone-pr-3", time.Minute, true),
		*ns("kube-system", "", time.Hour, false),
		*ns("pr-nolabel-4", "", time.Hour, true),
		*terminating,
	}
	got := SelectOrphans(list, map[string]bool{"live-pr-1": true}, now, 10*time.Minute)
	if strings.Join(got, ",") != "pr-nolabel-4,pr-orphan-2" {
		t.Fatalf("got %v", got)
	}
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSweepDeletesOnlyOrphans(t *testing.T) {
	live := &v1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "live-pr-1"}}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		live,
		ns("pr-live-1", "live-pr-1", time.Hour, true),
		ns("pr-orphan-2", "gone-pr-2", time.Hour, true),
		ns("default", "", time.Hour, false),
	).Build()
	col := &Collector{Client: c, Grace: 10 * time.Minute, Clock: clocktesting.NewFakePassiveClock(now), Log: logr.Discard()}
	deleted, err := col.Sweep(context.Background())
	if err != nil || strings.Join(deleted, ",") != "pr-orphan-2" {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	var remaining corev1.NamespaceList
	if err := c.List(context.Background(), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 2 {
		t.Fatalf("remaining %d", len(remaining.Items))
	}
	if !col.NeedLeaderElection() {
		t.Fatal("collector must be leader-only")
	}
}

func TestSweepErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, failOn := range []string{"NamespaceList", "PreviewEnvironmentList", "delete"} {
		c := fake.NewClientBuilder().WithScheme(scheme(t)).
			WithObjects(ns("pr-orphan-2", "gone", time.Hour, true)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					switch list.(type) {
					case *corev1.NamespaceList:
						if failOn == "NamespaceList" {
							return boom
						}
					case *v1alpha1.PreviewEnvironmentList:
						if failOn == "PreviewEnvironmentList" {
							return boom
						}
					}
					return cl.List(ctx, list, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if failOn == "delete" {
						return boom
					}
					return cl.Delete(ctx, obj, opts...)
				},
			}).Build()
		col := &Collector{Client: c, Clock: clocktesting.NewFakePassiveClock(now), Log: logr.Discard()}
		if _, err := col.Sweep(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("%s: got %v", failOn, err)
		}
	}
}

func TestStartStopsOnCancel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	col := &Collector{Client: c, Interval: time.Millisecond, Clock: clocktesting.NewFakePassiveClock(now), Log: logr.Discard()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := col.Start(ctx); err != nil {
		t.Fatal(err)
	}
	failing := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("x")
		},
	}).Build()
	col2 := &Collector{Client: failing, Clock: clocktesting.NewFakePassiveClock(now), Log: logr.Discard()}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := col2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
}
