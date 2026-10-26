package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/intake"
)

// GitHub event and delivery headers.
const (
	EventHeader    = "X-GitHub-Event"
	DeliveryHeader = "X-GitHub-Delivery"
)

// pullRequestPayload is the subset of GitHub's pull_request event that
// previewd reads. Unknown fields are ignored.
type pullRequestPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number    int       `json:"number"`
		State     string    `json:"state"`
		Draft     bool      `json:"draft"`
		Title     string    `json:"title"`
		UpdatedAt time.Time `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// relevantActions are the pull_request actions that can change what the
// preview should look like. Others (assigned, review_requested, ...) are
// acknowledged and ignored.
var relevantActions = map[string]bool{
	"opened": true, "reopened": true, "synchronize": true, "closed": true,
	"labeled": true, "unlabeled": true, "ready_for_review": true,
	"converted_to_draft": true, "edited": true,
}

// errIrrelevant marks payloads that are valid but need no action.
type errIrrelevant struct{ reason string }

func (e errIrrelevant) Error() string { return e.reason }

// ParsePullRequest decodes a pull_request payload into an intake event.
// It returns an errIrrelevant error (check with IsIrrelevant) for actions
// and pull requests previewd deliberately skips, such as fork PRs when
// allowForks is false.
func ParsePullRequest(body []byte, deliveryID string, allowForks bool) (intake.Event, error) {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return intake.Event{}, fmt.Errorf("decode pull_request payload: %w", err)
	}
	if !relevantActions[p.Action] {
		return intake.Event{}, errIrrelevant{reason: fmt.Sprintf("action %q does not affect previews", p.Action)}
	}
	owner, name := p.Repository.Owner.Login, p.Repository.Name
	if owner == "" || name == "" {
		owner, name, _ = strings.Cut(p.Repository.FullName, "/")
	}
	base := owner + "/" + name
	if !allowForks {
		// Fail closed: GitHub sends head.repo = null when the fork behind
		// a PR has been deleted, so a missing or nameless head repository
		// cannot be shown to be the base repository and counts as a fork.
		head := p.PullRequest.Head.Repo
		switch {
		case head == nil || head.FullName == "":
			return intake.Event{}, errIrrelevant{reason: "pull request head repository is unknown (deleted fork?); forks disabled"}
		case !strings.EqualFold(head.FullName, base):
			return intake.Event{}, errIrrelevant{reason: "pull request from fork " + head.FullName + " (forks disabled)"}
		}
	}
	number := p.PullRequest.Number
	if number == 0 {
		number = p.Number
	}
	state := v1alpha1.PRStateOpen
	if p.PullRequest.State == "closed" || p.Action == "closed" {
		state = v1alpha1.PRStateClosed
	}
	labels := make([]string, 0, len(p.PullRequest.Labels))
	for _, l := range p.PullRequest.Labels {
		labels = append(labels, l.Name)
	}
	ev := intake.Event{
		DeliveryID: deliveryID,
		Action:     p.Action,
		Repo:       v1alpha1.RepositoryRef{Owner: owner, Name: name},
		PR: v1alpha1.PullRequestRef{
			Number:  number,
			HeadSHA: p.PullRequest.Head.SHA,
			HeadRef: p.PullRequest.Head.Ref,
			BaseRef: p.PullRequest.Base.Ref,
			Author:  p.PullRequest.User.Login,
			Title:   p.PullRequest.Title,
			Labels:  labels,
			Draft:   p.PullRequest.Draft,
		},
		State:     state,
		UpdatedAt: p.PullRequest.UpdatedAt,
	}
	if err := ev.Validate(); err != nil {
		return intake.Event{}, fmt.Errorf("invalid pull_request payload: %w", err)
	}
	return ev, nil
}

// IsIrrelevant reports whether err means "valid, but nothing to do".
func IsIrrelevant(err error) bool {
	var e errIrrelevant
	return errors.As(err, &e)
}
