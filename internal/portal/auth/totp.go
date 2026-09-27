package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP settings: 30-second periods, 6 digits, SHA-1 (the only algorithm
// universally supported by authenticator apps).
const (
	totpPeriod = 30
	totpSkew   = 1
)

// NewTOTPKey generates a TOTP secret for a user.
func NewTOTPKey(issuer, account string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: account, Period: totpPeriod})
}

// ReplayGuard rejects TOTP codes already used in the current window, so an
// intercepted code cannot be reused.
type ReplayGuard struct {
	mu   sync.Mutex
	last map[string]uint64 // user ID -> last accepted time step
}

// NewReplayGuard returns an empty ReplayGuard.
func NewReplayGuard() *ReplayGuard {
	return &ReplayGuard{last: make(map[string]uint64)}
}

// Validate checks code against secret at time now, accepting one period of
// clock skew in either direction, and rejects reuse of a time step.
func (g *ReplayGuard) Validate(userID, secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	step := uint64(now.Unix()) / totpPeriod //nolint:gosec // time is after 1970
	for _, s := range []uint64{step, step - totpSkew, step + totpSkew} {
		t := time.Unix(int64(s*totpPeriod), 0) //nolint:gosec // small values
		ok, err := totp.ValidateCustom(code, secret, t, totp.ValidateOpts{
			Period: totpPeriod, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil || !ok {
			continue
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if s <= g.last[userID] {
			return false
		}
		g.last[userID] = s
		return true
	}
	return false
}

// Recovery codes are 10 base32 characters shown as two groups of five.
const recoveryCodeCount = 10

// NewRecoveryCodes returns fresh recovery codes and their hashes.
func NewRecoveryCodes() (codes []string, hashes [][]byte) {
	for range recoveryCodeCount {
		raw := strings.ToLower(rand.Text()[:10])
		codes = append(codes, raw[:5]+"-"+raw[5:])
		hashes = append(hashes, HashRecoveryCode(raw))
	}
	return codes, hashes
}

// HashRecoveryCode normalizes and hashes a recovery code.
func HashRecoveryCode(code string) []byte {
	code = strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(code))
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// LooksLikeRecoveryCode reports whether input has the shape of a recovery
// code rather than a TOTP code.
func LooksLikeRecoveryCode(input string) bool {
	s := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(input))
	return len(s) == 10
}
