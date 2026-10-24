// Package intake turns normalised pull-request events into desired state:
// it creates or updates the PreviewEnvironment for the pull request and
// nothing else. All side effects on namespaces and the SCM happen later, in
// the reconciler, which is what keeps webhook handling fast, idempotent and
// safe to retry.
package intake

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/archit2501/previewd/api/v1alpha1"
	"github.com/archit2501/previewd/internal/naming"
)

// ErrDeleting is returned when the PreviewEnvironment for an event is still
// being deleted; the caller should ask the sender to retry.
var ErrDeleting = errors.New("previous preview is still being deleted; retry later")

// Event is an SCM-neutral pull-request event.
type Event struct {
	// DeliveryID is the SCM's unique delivery identifier, for logs.
	DeliveryID string
	// Action is the SCM action (opened, synchronize, closed, ...).
	Action string
	Repo   v1alpha1.RepositoryRef
	PR     v1alpha1.PullRequestRef
	State  v1alpha1.PRState
	// UpdatedAt is the pull request's updated_at. Events older than what
	// the PreviewEnvironment already reflects are ignored.
	UpdatedAt time.Time
}

// Outcome classifies what Apply did.
type Outcome string

// Apply outcomes.
const (
	OutcomeCreated   Outcome = "created"
	OutcomeUpdated   Outcome = "updated"
	OutcomeUnchanged Outcome = "unchanged"
	OutcomeStale     Outcome = "stale"
	OutcomeIgnored   Outcome = "ignored"
)

// Result describes the effect of one event.
type Result struct {
	Outcome  Outcome
	Instance string
	Reason   string
}

// Rule maps repositories to a template set. Pattern is matched against
// "owner/name" with path.Match semantics, so "acme/*" matches every
// repository of acme.
type Rule struct {
	Pattern  string
	Template string
}

// ParseRules parses "pattern=template" entries; a bare pattern uses
// defaultTemplate.
func ParseRules(entries []string, defaultTemplate string) ([]Rule, error) {
	var rules []Rule
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		pattern, tpl, found := strings.Cut(e, "=")
		if !found || tpl == "" {
			tpl = defaultTemplate
		}
		if _, err := path.Match(pattern, "x/y"); err != nil || !strings.Contains(pattern, "/") {
			return nil, fmt.Errorf("invalid repository pattern %q: want owner/name, owner/* or */*", pattern)
		}
		rules = append(rules, Rule{Pattern: strings.ToLower(pattern), Template: tpl})
	}
	if len(rules) == 0 {
		return nil, errors.New("at least one repository rule is required")
	}
	return rules, nil
}

// Match returns the template for repo, or false if no rule allows it.
func Match(rules []Rule, repo v1alpha1.RepositoryRef) (string, bool) {
	full := strings.ToLower(repo.FullName())
	for _, r := range rules {
		if ok, _ := path.Match(r.Pattern, full); ok {
			return r.Template, true
		}
	}
	return "", false
}

// Sink writes PreviewEnvironments.
type Sink struct {
	// Client writes PreviewEnvironments.
	Client client.Client
	// Reader reads them; pass an uncached reader (the manager's API
	// reader) so the stale-event check compares against the latest spec.
	// Nil means Client.
	Reader client.Reader
	Rules  []Rule
	Log    logr.Logger
}

// Validate checks the event carries what a PreviewEnvironment needs.
func (e Event) Validate() error {
	var errs []error
	if e.Repo.Owner == "" || e.Repo.Name == "" {
		errs = append(errs, errors.New("repository owner and name are required"))
	}
	if e.PR.Number < 1 {
		errs = append(errs, errors.New("pull request number must be positive"))
	}
	if !isHexSHA(e.PR.HeadSHA) {
		errs = append(errs, fmt.Errorf("head SHA %q is not a hex commit id", e.PR.HeadSHA))
	}
	if e.State != v1alpha1.PRStateOpen && e.State != v1alpha1.PRStateClosed {
		errs = append(errs, fmt.Errorf("unknown state %q", e.State))
	}
	if e.UpdatedAt.IsZero() {
		errs = append(errs, errors.New("updated_at is required"))
	}
	return errors.Join(errs...)
}

func isHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Apply upserts the PreviewEnvironment for ev. It is idempotent: applying
// the same event twice yields OutcomeUnchanged the second time, and an event
// older than the stored spec yields OutcomeStale without writing.
func (s *Sink) Apply(ctx context.Context, ev Event) (Result, error) {
	if err := ev.Validate(); err != nil {
		return Result{}, err
	}
	instance := naming.Instance(ev.Repo.Owner, ev.Repo.Name, ev.PR.Number)
	template, allowed := Match(s.Rules, ev.Repo)
	if !allowed {
		return Result{Outcome: OutcomeIgnored, Instance: instance, Reason: "repository not allowed"}, nil
	}
	var res Result
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error
		res, err = s.upsert(ctx, instance, template, ev)
		return err
	})
	if err != nil {
		return Result{}, fmt.Errorf("upsert %s: %w", instance, err)
	}
	s.Log.Info("intake", "delivery", ev.DeliveryID, "action", ev.Action, "instance", instance,
		"outcome", res.Outcome, "reason", res.Reason, "sha", ev.PR.HeadSHA)
	return res, nil
}

func (s *Sink) upsert(ctx context.Context, instance, template string, ev Event) (Result, error) {
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	var pe v1alpha1.PreviewEnvironment
	err := reader.Get(ctx, types.NamespacedName{Name: instance}, &pe)
	switch {
	case apierrors.IsNotFound(err):
		if ev.State == v1alpha1.PRStateClosed {
			return Result{Outcome: OutcomeIgnored, Instance: instance, Reason: "closed pull request has no preview"}, nil
		}
		pe = v1alpha1.PreviewEnvironment{
			ObjectMeta: metav1.ObjectMeta{
				Name:   instance,
				Labels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue},
			},
			Spec: specFrom(ev, template),
		}
		if err := s.Client.Create(ctx, &pe); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race with a concurrent delivery; retry as an update.
				return Result{}, apierrors.NewConflict(v1alpha1.GroupVersion.WithResource("previewenvironments").GroupResource(), instance, err)
			}
			return Result{}, err
		}
		return Result{Outcome: OutcomeCreated, Instance: instance}, nil
	case err != nil:
		return Result{}, err
	}
	if !pe.DeletionTimestamp.IsZero() {
		// The old record is being torn down; GitHub will send another event
		// on the next push, and the reconcile loop must not be confused by a
		// spec change mid-deletion.
		return Result{}, fmt.Errorf("preview %s: %w", instance, ErrDeleting)
	}
	if ev.UpdatedAt.Before(pe.Spec.LastActivityTime.Time) {
		return Result{Outcome: OutcomeStale, Instance: instance,
			Reason: "event older than " + pe.Spec.LastActivityTime.UTC().Format(time.RFC3339)}, nil
	}
	want := specFrom(ev, pe.Spec.Template)
	want.TTL = pe.Spec.TTL // operator overrides survive events
	if specEqual(pe.Spec, want) {
		return Result{Outcome: OutcomeUnchanged, Instance: instance}, nil
	}
	pe.Spec = want
	if err := s.Client.Update(ctx, &pe); err != nil {
		return Result{}, err
	}
	return Result{Outcome: OutcomeUpdated, Instance: instance}, nil
}

func specFrom(ev Event, template string) v1alpha1.PreviewEnvironmentSpec {
	pr := ev.PR
	pr.Labels = append([]string(nil), pr.Labels...)
	sort.Strings(pr.Labels)
	return v1alpha1.PreviewEnvironmentSpec{
		Repository:       ev.Repo,
		PullRequest:      pr,
		State:            ev.State,
		Template:         template,
		LastActivityTime: metav1.NewTime(ev.UpdatedAt.UTC().Truncate(time.Second)),
	}
}

func specEqual(a, b v1alpha1.PreviewEnvironmentSpec) bool {
	if a.Repository != b.Repository || a.State != b.State || a.Template != b.Template ||
		!a.LastActivityTime.Equal(&b.LastActivityTime) {
		return false
	}
	if (a.TTL == nil) != (b.TTL == nil) || (a.TTL != nil && a.TTL.Duration != b.TTL.Duration) {
		return false
	}
	pa, pb := a.PullRequest, b.PullRequest
	if pa.Number != pb.Number || pa.HeadSHA != pb.HeadSHA || pa.HeadRef != pb.HeadRef ||
		pa.BaseRef != pb.BaseRef || pa.Author != pb.Author || pa.Title != pb.Title ||
		pa.Draft != pb.Draft || len(pa.Labels) != len(pb.Labels) {
		return false
	}
	for i := range pa.Labels {
		if pa.Labels[i] != pb.Labels[i] {
			return false
		}
	}
	return true
}
