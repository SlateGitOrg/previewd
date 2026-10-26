// Package scm reports preview status back to the source-control system.
//
// The reconciler depends only on the Reporter interface, so the GitHub
// implementation, the in-memory Recorder used by tests and the demo, and
// the log-only reporter are interchangeable.
package scm

import (
	"context"
	"sync"

	"github.com/go-logr/logr"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/lifecycle"
)

// Status is one status update for a commit.
type Status struct {
	Repo        v1alpha1.RepositoryRef
	SHA         string
	State       lifecycle.ReportState
	URL         string
	Description string
}

// Reporter posts statuses. Implementations must be safe for concurrent use.
type Reporter interface {
	Report(ctx context.Context, s Status) error
}

// Recorder is an in-memory Reporter for tests and demos. It records every
// status and can be told to fail.
type Recorder struct {
	mu       sync.Mutex
	statuses []Status
	failures int
	err      error
}

// FailNext makes the next n Report calls return err.
func (r *Recorder) FailNext(n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures, r.err = n, err
}

// Report records s, or fails if FailNext is pending.
func (r *Recorder) Report(_ context.Context, s Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures > 0 {
		r.failures--
		return r.err
	}
	r.statuses = append(r.statuses, s)
	return nil
}

// Statuses returns a copy of the recorded statuses.
func (r *Recorder) Statuses() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Status(nil), r.statuses...)
}

// LogReporter only logs; it is used when no SCM token is configured.
type LogReporter struct{ Log logr.Logger }

// Report logs s.
func (l LogReporter) Report(_ context.Context, s Status) error {
	l.Log.Info("status (not posted: no SCM token configured)", "repo", s.Repo.FullName(),
		"sha", s.SHA, "state", s.State, "url", s.URL, "description", s.Description)
	return nil
}
