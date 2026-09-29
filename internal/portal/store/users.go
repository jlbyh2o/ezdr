package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrConflict is returned when a record would violate a uniqueness constraint.
var ErrConflict = errors.New("already exists")

// ErrSetupComplete is returned when creating the first user after one exists.
var ErrSetupComplete = errors.New("setup already complete")

// User is a portal user.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	// TOTPSecret is the encrypted TOTP secret, or nil if TOTP is not set up.
	TOTPSecret []byte
	CreatedAt  time.Time
}

// NewID returns a random, URL-safe identifier.
func NewID() string {
	return strings.ToLower(rand.Text())
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

// CreateFirstUser creates a user only if no users exist yet.
func (s *Store) CreateFirstUser(ctx context.Context, username, passwordHash string) (User, error) {
	u := User{ID: NewID(), Username: username, PasswordHash: passwordHash, CreatedAt: s.now().UTC()}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupComplete
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)",
			u.ID, u.Username, u.PasswordHash, toMillis(u.CreatedAt))
		return err
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

const userColumns = "id, username, password_hash, totp_secret, created_at"

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.CreatedAt = fromMillis(created)
	return u, nil
}

// UserByUsername looks up a user by username, ignoring case.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE username = ?", username))
}

// UserByID looks up a user by ID.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

// SetTOTP stores the encrypted TOTP secret of a user who has none yet and
// replaces their recovery codes with the given hashes. It returns
// ErrNotFound if the user doesn't exist or already has a secret.
func (s *Store) SetTOTP(ctx context.Context, userID string, sealedSecret []byte, recoveryCodeHashes [][]byte) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE users SET totp_secret = ? WHERE id = ? AND totp_secret IS NULL", sealedSecret, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ?", userID); err != nil {
			return err
		}
		for _, h := range recoveryCodeHashes {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)", userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseRecoveryCode marks an unused recovery code as used. It reports whether a
// matching unused code was found.
func (s *Store) UseRecoveryCode(ctx context.Context, userID string, codeHash []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL",
		toMillis(s.now()), userID, codeHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
