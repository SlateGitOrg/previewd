package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/SlateGitOrg/previewd/api/v1alpha1"
	"github.com/SlateGitOrg/previewd/internal/naming"
)

// Input is what a render needs to know about one pull request.
type Input struct {
	// Instance is the PreviewEnvironment name.
	Instance    string
	Repo        v1alpha1.RepositoryRef
	PullRequest v1alpha1.PullRequestRef
}

// InputFrom builds the render input for a PreviewEnvironment.
//
// The TTL deliberately is not an input: it moves on every PR event, and
// rendering it would make every label or title edit re-apply the bundle.
// The controller maintains the expiry label on the namespace separately.
func InputFrom(pe *v1alpha1.PreviewEnvironment) Input {
	return Input{
		Instance:    pe.Name,
		Repo:        pe.Spec.Repository,
		PullRequest: pe.Spec.PullRequest,
	}
}

// Values is the data model exposed to templates. Every field derived from
// the pull request is attacker-influenced (branch names, titles, logins):
// templates must pass free-text fields through `quote` and should prefer the
// pre-sanitised *Slug fields in names.
type Values struct {
	Instance  string
	Namespace string
	Host      string
	URL       string
	Repo      RepoValues
	PR        PRValues
	Vars      map[string]string
}

// RepoValues describes the repository.
type RepoValues struct {
	Owner    string
	Name     string
	FullName string
	Slug     string
}

// PRValues describes the pull request.
type PRValues struct {
	Number      int
	HeadSHA     string
	ShortSHA    string
	HeadRef     string
	HeadRefSlug string
	BaseRef     string
	Author      string
	Title       string
	Labels      []string
}

func (t *TemplateSet) values(in Input) (Values, error) {
	sha := in.PullRequest.HeadSHA
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	v := Values{
		Instance:  in.Instance,
		Namespace: naming.Namespace(t.Config.NamespacePrefix, in.Repo.Name, in.PullRequest.Number),
		Repo: RepoValues{
			Owner:    in.Repo.Owner,
			Name:     in.Repo.Name,
			FullName: in.Repo.FullName(),
			Slug:     naming.Slug(in.Repo.Name),
		},
		PR: PRValues{
			Number:      in.PullRequest.Number,
			HeadSHA:     sha,
			ShortSHA:    short,
			HeadRef:     in.PullRequest.HeadRef,
			HeadRefSlug: naming.Slug(in.PullRequest.HeadRef),
			BaseRef:     in.PullRequest.BaseRef,
			Author:      in.PullRequest.Author,
			Title:       in.PullRequest.Title,
			Labels:      append([]string(nil), in.PullRequest.Labels...),
		},
		Vars: map[string]string{},
	}
	for k, val := range t.Config.Vars {
		v.Vars[k] = val
	}
	if t.host != nil {
		var b strings.Builder
		if err := t.host.Execute(&b, v); err != nil {
			return Values{}, fmt.Errorf("host template: %w", err)
		}
		v.Host = strings.TrimSpace(b.String())
		v.URL = t.Config.URLScheme + "://" + v.Host
	}
	return v, nil
}

// funcMap is the deliberately small set of template helpers. It avoids a
// general-purpose library so the rendering surface stays auditable.
func funcMap() template.FuncMap {
	return template.FuncMap{
		// quote renders s as a double-quoted YAML/JSON scalar, escaping
		// newlines, quotes and control characters.
		"quote": func(s any) (string, error) {
			b, err := json.Marshal(fmt.Sprint(s))
			return string(b), err
		},
		"dnsLabel": func(s string) string {
			out := naming.Slug(s)
			if len(out) > naming.MaxLabelLength {
				out = strings.TrimRight(out[:naming.MaxLabelLength], "-")
			}
			return out
		},
		"labelValue": naming.LabelValue,
		"lower":      strings.ToLower,
		"upper":      strings.ToUpper,
		"trunc": func(n int, s string) string {
			if n >= 0 && len(s) > n {
				return s[:n]
			}
			return s
		},
		"default": func(def, v string) string {
			if v == "" {
				return def
			}
			return v
		},
		"replace": func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
		"join":    func(sep string, xs []string) string { return strings.Join(xs, sep) },
		"has": func(needle string, xs []string) bool {
			for _, x := range xs {
				if x == needle {
					return true
				}
			}
			return false
		},
	}
}
