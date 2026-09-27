// Package auth implements portal user authentication: password hashing, TOTP,
// sign-in challenges, sessions, and rate limiting.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLength is the minimum password length in characters.
const MinPasswordLength = 12

// Argon2id parameters (64 MiB, 2 passes, 2 lanes).
const (
	argonMemory  = 64 * 1024
	argonTime    = 2
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ValidatePassword checks a new password against the password policy.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

// HashPassword returns an Argon2id hash in PHC string format.
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// dummyHash is verified against when a user does not exist, so that unknown
// usernames take as long to reject as wrong passwords.
var dummyHash = HashPassword("not a real password, only used for timing")

// VerifyPassword reports whether password matches hash. An empty hash is
// treated as a nonexistent user.
func VerifyPassword(password, hash string) bool {
	if hash == "" {
		_, _ = verify(password, dummyHash)
		return false
	}
	ok, err := verify(password, hash)
	return err == nil && ok
}

func verify(password, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var memory, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(want))) //nolint:gosec // length is 32
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
