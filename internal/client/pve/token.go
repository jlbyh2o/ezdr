package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnsureToken makes sure the read-only API user and token exist and that
// TokenFile holds a working secret. It reports whether it created a token.
func EnsureToken(ctx context.Context) (bool, error) {
	if c, err := NewClient(); err == nil {
		var v map[string]any
		if err := c.Get(ctx, "version", &v); err == nil {
			return false, nil
		}
	}

	users, err := pveum(ctx, "user", "list", "--output-format", "json")
	if err != nil {
		return false, err
	}
	var list []struct {
		UserID string `json:"userid"`
	}
	if err := json.Unmarshal(users, &list); err != nil {
		return false, fmt.Errorf("parse user list: %w", err)
	}
	exists := false
	for _, u := range list {
		if u.UserID == User {
			exists = true
		}
	}
	if !exists {
		if _, err := pveum(ctx, "user", "add", User, "--comment", "EZDR client (read-only inventory)"); err != nil {
			return false, err
		}
	}
	// With privilege separation a token's permissions are the intersection
	// of its own and its user's, so both get the role.
	if _, err := pveum(ctx, "acl", "modify", "/", "--users", User, "--roles", Role); err != nil {
		return false, err
	}
	// The secret is only shown at creation, so replace any existing token.
	_, _ = pveum(ctx, "user", "token", "remove", User, TokenName)
	out, err := pveum(ctx, "user", "token", "add", User, TokenName,
		"--privsep", "1", "--comment", "EZDR inventory", "--output-format", "json")
	if err != nil {
		return false, err
	}
	var tok struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &tok); err != nil || tok.Value == "" {
		return false, errors.New("could not read the new API token secret")
	}
	if _, err := pveum(ctx, "acl", "modify", "/", "--tokens", TokenID, "--roles", Role); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(TokenFile), 0o700); err != nil {
		return false, err
	}
	if err := os.WriteFile(TokenFile, []byte(TokenID+"="+tok.Value+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveToken deletes the API user, which also removes its token and
// permissions. A missing user is not an error.
func RemoveToken(ctx context.Context) error {
	_, err := pveum(ctx, "user", "delete", User)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

func pveum(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "pveum", args...) //nolint:gosec // fixed binary; arguments come from this package
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pveum %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
