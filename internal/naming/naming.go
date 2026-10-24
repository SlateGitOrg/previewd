// Package naming derives stable, DNS-safe Kubernetes names from repository
// and pull-request identifiers.
//
// Every name produced here is deterministic: the same (owner, repo, number)
// always maps to the same PreviewEnvironment and namespace, which is what lets
// the webhook intake and the reconciler find each other's objects without a
// lookup table.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	// MaxLabelLength is the DNS-1123 label limit (namespaces, label values).
	MaxLabelLength = 63
	// MaxSubdomainLength is the DNS-1123 subdomain limit (most object names).
	MaxSubdomainLength = 253
	hashLength         = 6
)

// Slug lowercases s and replaces every run of characters outside [a-z0-9]
// with a single '-', trimming leading and trailing dashes. It returns "x"
// for input with no usable characters so callers never build empty segments.
func Slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "x"
	}
	return out
}

// ShortHash returns the first n hex characters of sha256(s).
func ShortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

// fit joins prefix, a variable middle and a fixed suffix into at most max
// characters. When the result would be too long, the middle is truncated and
// a short hash of the untruncated middle is inserted so distinct inputs stay
// distinct.
func fit(prefix, middle, suffix string, maxLen int) string {
	full := prefix + middle + suffix
	if len(full) <= maxLen {
		return full
	}
	hash := ShortHash(middle, hashLength)
	room := maxLen - len(prefix) - len(suffix) - len(hash) - 1
	if room < 1 {
		// Prefix and suffix alone are too long; fall back to a pure hash
		// name, which is still deterministic and valid.
		return "h" + ShortHash(full, maxLen-1)
	}
	trimmed := strings.TrimRight(middle[:room], "-")
	return prefix + trimmed + "-" + hash + suffix
}

// Namespace returns the namespace for a pull request, e.g.
// "pr-acme-shop-42". The result is a valid DNS-1123 label.
func Namespace(prefix, repo string, number int) string {
	p := Slug(prefix) + "-"
	return fit(p, Slug(repo), "-"+strconv.Itoa(number), MaxLabelLength)
}

// Instance returns the PreviewEnvironment name for a pull request, e.g.
// "acme-shop-pr-42". It is a valid DNS-1123 label (so it can also be used as
// a label value).
func Instance(owner, repo string, number int) string {
	return fit("", Slug(owner)+"-"+Slug(repo), "-pr-"+strconv.Itoa(number), MaxLabelLength)
}

// LabelValue makes s usable as a Kubernetes label value: at most 63
// characters of [A-Za-z0-9._-], starting and ending alphanumeric.
func LabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > MaxLabelLength {
		out = out[:MaxLabelLength-hashLength-1] + "-" + ShortHash(s, hashLength)
	}
	return strings.Trim(out, "._-")
}
