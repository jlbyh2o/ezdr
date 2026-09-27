package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestPassword(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	if !VerifyPassword("correct horse battery", h) {
		t.Error("correct password rejected")
	}
	if VerifyPassword("wrong horse battery", h) {
		t.Error("wrong password accepted")
	}
	if VerifyPassword("anything", "") {
		t.Error("missing user accepted")
	}
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
}

func TestTOTPReplay(t *testing.T) {
	key, err := NewTOTPKey("EZDR", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code, _ := totp.GenerateCode(key.Secret(), now)
	g := NewReplayGuard()
	if !g.Validate("u1", key.Secret(), code, now) {
		t.Fatal("valid code rejected")
	}
	if g.Validate("u1", key.Secret(), code, now) {
		t.Error("replayed code accepted")
	}
	wrong := []byte(code)
	wrong[0] = '0' + (wrong[0]-'0'+1)%10
	if g.Validate("u2", key.Secret(), string(wrong), now) {
		t.Error("wrong code accepted")
	}
	later := now.Add(2 * time.Minute)
	next, _ := totp.GenerateCode(key.Secret(), later)
	if !g.Validate("u1", key.Secret(), next, later) {
		t.Error("new code in a later window rejected")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes()
	if len(codes) != recoveryCodeCount || len(hashes) != recoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	if !LooksLikeRecoveryCode(codes[0]) || LooksLikeRecoveryCode("123456") {
		t.Error("recovery code detection is wrong")
	}
	if string(HashRecoveryCode(strings.ToUpper(codes[0]))) != string(hashes[0]) {
		t.Error("recovery code hashing is not normalized")
	}
}

func TestChallenges(t *testing.T) {
	c := NewChallenges()
	id := c.Create(Challenge{UserID: "u1"})
	for i := range maxChallengeAttempts {
		if _, ok := c.Attempt(id); !ok {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	if _, ok := c.Attempt(id); ok {
		t.Error("attempt beyond the limit accepted")
	}
	if _, ok := c.Attempt("missing"); ok {
		t.Error("unknown challenge accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(time.Hour, 2)
	for i := range 2 {
		if !r.Allow("k") {
			t.Fatalf("event %d within burst rejected", i+1)
		}
	}
	if r.Allow("k") {
		t.Error("limit not enforced")
	}
	if !r.Allow("other") {
		t.Error("keys are not independent")
	}
}
