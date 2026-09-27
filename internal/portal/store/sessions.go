package store

import (
	"context"
	"time"
)

// CreateSession stores a session. Only a hash of the session ID is stored.
func (s *Store) CreateSession(ctx context.Context, idHash []byte, userID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO sessions (id_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		idHash, userID, toMillis(s.now()), toMillis(expiresAt))
	return err
}

// SessionUser returns the user for an unexpired session.
func (s *Store) SessionUser(ctx context.Context, idHash []byte) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT u.id, u.username, u.password_hash, u.totp_secret, u.created_at "+
			"FROM sessions s JOIN users u ON u.id = s.user_id "+
			"WHERE s.id_hash = ? AND s.expires_at > ?",
		idHash, toMillis(s.now())))
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", idHash)
	return err
}

// DeleteExpiredSessions removes all expired sessions.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", toMillis(s.now()))
	return err
}
