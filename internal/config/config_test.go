package config

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func files(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
}

const goodSecret = "0123456789abcdef-secret"

func TestDefaults(t *testing.T) {
	c, err := Parse([]string{"--repos", "acme/*"}, env(map[string]string{EnvWebhookSecrets: goodSecret}), files(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.WebhookAddr != ":8090" || c.PollInterval != 10*time.Second || c.Retention != 24*time.Hour ||
		len(c.Rules) != 1 || c.Rules[0].Template != "default" || c.GitHubToken != "" || len(c.WebhookSecrets) != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestFullFlags(t *testing.T) {
	args := []string{
		"--repos", "acme/shop=default, acme/site=static-site", "--repos", "other/*",
		"--webhook-secret-file", "/s", "--github-token-file", "/t",
		"--required-label", "preview", "--skip-drafts", "--allow-forks", "--leader-elect",
		"--retention", "0", "--poll-interval", "3s",
	}
	c, err := Parse(args, env(nil), files(map[string]string{
		"/s": goodSecret + "\n\nanother-secret-0123456\n",
		"/t": " ghp_token \n",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 3 || c.Rules[1].Template != "static-site" || len(c.WebhookSecrets) != 2 ||
		c.GitHubToken != "ghp_token" || !c.SkipDrafts || !c.AllowForks || !c.LeaderElect || c.Retention != 0 ||
		c.RequiredLabel != "preview" || c.PollInterval != 3*time.Second {
		t.Fatalf("%+v", c)
	}
}

func TestTokenFromEnv(t *testing.T) {
	c, err := Parse([]string{"--repos", "a/b"}, env(map[string]string{EnvWebhookSecrets: goodSecret, EnvGitHubToken: "tok"}), files(nil), io.Discard)
	if err != nil || c.GitHubToken != "tok" {
		t.Fatalf("%v %q", err, c.GitHubToken)
	}
}

func TestErrors(t *testing.T) {
	cases := map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no repos":       {nil, map[string]string{EnvWebhookSecrets: goodSecret}, "--repos"},
		"no secret":      {[]string{"--repos", "a/b"}, nil, "no webhook secret"},
		"short secret":   {[]string{"--repos", "a/b"}, map[string]string{EnvWebhookSecrets: "short"}, "at least 16"},
		"empty file":     {[]string{"--repos", "a/b", "--webhook-secret-file", "/empty"}, nil, "contains no secrets"},
		"missing file":   {[]string{"--repos", "a/b", "--webhook-secret-file", "/nope"}, nil, "--webhook-secret-file"},
		"missing token":  {[]string{"--repos", "a/b", "--github-token-file", "/nope"}, map[string]string{EnvWebhookSecrets: goodSecret}, "--github-token-file"},
		"bad duration":   {[]string{"--repos", "a/b", "--poll-interval", "0s"}, map[string]string{EnvWebhookSecrets: goodSecret}, "--poll-interval"},
		"negative":       {[]string{"--repos", "a/b", "--retention", "-1h"}, map[string]string{EnvWebhookSecrets: goodSecret}, "negative"},
		"zero workers":   {[]string{"--repos", "a/b", "--max-concurrent-reconciles", "0"}, map[string]string{EnvWebhookSecrets: goodSecret}, "positive"},
		"bad path":       {[]string{"--repos", "a/b", "--webhook-path", "hook"}, map[string]string{EnvWebhookSecrets: goodSecret}, "start with /"},
		"unknown flag":   {[]string{"--nope"}, nil, "not defined"},
		"positional arg": {[]string{"--repos", "a/b", "extra"}, map[string]string{EnvWebhookSecrets: goodSecret}, "unexpected"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.args, env(tc.env), files(map[string]string{"/empty": "\n"}), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFromOS(t *testing.T) {
	t.Setenv(EnvWebhookSecrets, goodSecret)
	if _, err := FromOS([]string{"--repos", "a/b"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var l listFlag
	_ = l.Set("a, b,,c")
	if l.String() != "a,b,c" {
		t.Fatal(l.String())
	}
}
