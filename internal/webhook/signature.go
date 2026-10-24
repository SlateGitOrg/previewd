// Package webhook receives GitHub-format pull_request webhooks, verifies
// their HMAC signatures and hands normalised events to the intake.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader is the header GitHub uses for the HMAC-SHA256 signature.
const SignatureHeader = "X-Hub-Signature-256"

const signaturePrefix = "sha256="

// Signature errors. They are deliberately coarse in HTTP responses; the
// distinction is only logged.
var (
	ErrMissingSignature   = errors.New("missing " + SignatureHeader + " header")
	ErrMalformedSignature = errors.New("malformed signature")
	ErrSignatureMismatch  = errors.New("signature does not match any configured secret")
	ErrNoSecrets          = errors.New("no webhook secrets configured")
)

// Sign returns the X-Hub-Signature-256 value for body under secret.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks header against body for any of secrets. Supporting several
// secrets allows zero-downtime rotation: configure new and old, switch the
// SCM to the new secret, then drop the old one.
//
// The comparison is constant-time with respect to the signature bytes, and
// every configured secret is always tried so timing does not reveal which
// secret (if any) was close.
func Verify(secrets [][]byte, body []byte, header string) error {
	if len(secrets) == 0 {
		return ErrNoSecrets
	}
	if header == "" {
		return ErrMissingSignature
	}
	if !strings.HasPrefix(header, signaturePrefix) {
		return ErrMalformedSignature
	}
	got, err := hex.DecodeString(header[len(signaturePrefix):])
	if err != nil || len(got) != sha256.Size {
		return ErrMalformedSignature
	}
	ok := false
	for _, s := range secrets {
		mac := hmac.New(sha256.New, s)
		mac.Write(body)
		if hmac.Equal(got, mac.Sum(nil)) {
			ok = true
		}
	}
	if !ok {
		return ErrSignatureMismatch
	}
	return nil
}
