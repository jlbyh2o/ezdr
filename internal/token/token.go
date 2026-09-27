// Package token encodes and decodes enrollment token strings.
//
// A token string is "ezdr1_" followed by the base64url-encoded (unpadded)
// JSON payload. The prefix identifies the format version and makes tokens
// easy for secret scanners to detect.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Prefix identifies version 1 token strings.
const Prefix = "ezdr1_"

// SecretSize is the length of a token secret in bytes.
const SecretSize = 32

// Token is the content of an enrollment token.
type Token struct {
	// PortalURL is the portal's public base URL, for example
	// "https://portal.example.com".
	PortalURL string `json:"u"`
	// ID identifies the token record in the portal.
	ID string `json:"i"`
	// Secret proves possession of the token.
	Secret []byte `json:"s"`
	// TLSPin, if set, is the base64-encoded SHA-256 hash of the
	// SubjectPublicKeyInfo of a certificate the portal presents. The client
	// then trusts only certificate chains containing that key.
	TLSPin string `json:"p,omitempty"`
}

// NewSecret returns a random token secret.
func NewSecret() []byte {
	b := make([]byte, SecretSize)
	_, _ = rand.Read(b)
	return b
}

// HashSecret returns the hash of a secret as stored by the portal.
func HashSecret(secret []byte) []byte {
	h := sha256.Sum256(secret)
	return h[:]
}

// Encode returns the token string.
func (t Token) Encode() string {
	b, _ := json.Marshal(t) //nolint:gosec // the token string is meant to carry its secret
	return Prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Decode parses a token string.
func Decode(s string) (Token, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, Prefix) {
		return Token{}, errors.New("not an EZDR enrollment token")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil {
		return Token{}, errors.New("malformed enrollment token")
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, errors.New("malformed enrollment token")
	}
	if t.ID == "" || len(t.Secret) != SecretSize {
		return Token{}, errors.New("incomplete enrollment token")
	}
	u, err := url.Parse(t.PortalURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Token{}, errors.New("enrollment token has an invalid portal URL (HTTPS is required)")
	}
	return t, nil
}
