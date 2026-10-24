package scm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/archit2501/previewd/internal/lifecycle"
)

// DefaultGitHubAPI is the public GitHub REST endpoint. GitHub Enterprise
// Server uses https://HOST/api/v3.
const DefaultGitHubAPI = "https://api.github.com"

// GitHub posts commit statuses through the REST API
// (POST /repos/{owner}/{repo}/statuses/{sha}).
type GitHub struct {
	// BaseURL is the API root, e.g. DefaultGitHubAPI.
	BaseURL string
	// Token is a token with commit-status write access.
	Token string
	// Context is the status context shown in the PR checks list.
	Context string
	// HTTP is the client to use; nil means a client with a 10s timeout.
	HTTP *http.Client
	// MaxAttempts bounds retries of 5xx/429 responses (default 3).
	MaxAttempts int
	// Backoff is the base delay between attempts (default 500ms).
	Backoff time.Duration
	// Sleep is injectable for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// githubState maps previewd states onto GitHub's commit-status vocabulary
// (error, failure, pending, success).
func githubState(s lifecycle.ReportState) (string, string) {
	switch s {
	case lifecycle.ReportPending:
		return "pending", "Preview environment is being provisioned"
	case lifecycle.ReportReady:
		return "success", "Preview environment is ready"
	case lifecycle.ReportFailed:
		return "failure", "Preview environment failed to render or apply"
	case lifecycle.ReportExpired:
		return "error", "Preview expired (TTL); push a commit to recreate it"
	default:
		// The preview was removed on purpose (PR closed, label removed):
		// not a failure of the change under review.
		return "success", "Preview environment removed"
	}
}

type statusRequest struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

// APIError is a non-retryable error response from GitHub.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: HTTP %d: %s", e.StatusCode, e.Message)
}

// Report posts s as a commit status, retrying transient failures.
func (g *GitHub) Report(ctx context.Context, s Status) error {
	state, desc := githubState(s.State)
	if s.Description != "" {
		desc = s.Description
	}
	// GitHub rejects descriptions longer than 140 characters.
	if len(desc) > 140 {
		desc = desc[:137] + "..."
	}
	statusCtx := g.Context
	if statusCtx == "" {
		statusCtx = "previewd"
	}
	payload, err := json.Marshal(statusRequest{State: state, TargetURL: s.URL, Description: desc, Context: statusCtx})
	if err != nil {
		return err
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = DefaultGitHubAPI
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", base,
		url.PathEscape(s.Repo.Owner), url.PathEscape(s.Repo.Name), url.PathEscape(s.SHA))

	attempts := g.MaxAttempts
	if attempts < 1 {
		attempts = 3
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryAfter, err := g.post(ctx, endpoint, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		var apiErr *APIError
		if errors.As(err, &apiErr) || attempt == attempts {
			break
		}
		delay := g.backoff(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if err := g.sleep(ctx, delay); err != nil {
			return err
		}
	}
	return fmt.Errorf("report status for %s@%s: %w", s.Repo.FullName(), s.SHA, lastErr)
}

// post sends one request. A transient failure returns a plain error and an
// optional Retry-After; a permanent one returns *APIError.
func (g *GitHub) post(ctx context.Context, endpoint string, payload []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, &APIError{Message: err.Error()}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "previewd")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0, nil
	}
	msg := strings.TrimSpace(string(body))
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		msg = parsed.Message
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0") {
		var after time.Duration
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		return after, fmt.Errorf("github: transient HTTP %d: %s", resp.StatusCode, msg)
	}
	return 0, &APIError{StatusCode: resp.StatusCode, Message: msg}
}

func (g *GitHub) backoff(attempt int) time.Duration {
	base := g.Backoff
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	return base << (attempt - 1)
}

func (g *GitHub) sleep(ctx context.Context, d time.Duration) error {
	if g.Sleep != nil {
		return g.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
