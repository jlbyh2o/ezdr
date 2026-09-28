package store

import (
	"context"
	"database/sql"
	"errors"
)

// PutDNSCheck stores a plan's latest DNS check.
func (s *Store) PutDNSCheck(ctx context.Context, planID string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dns_checks (plan_id, data, checked_at) VALUES (?, ?, ?)
		ON CONFLICT (plan_id) DO UPDATE SET data = excluded.data, checked_at = excluded.checked_at`,
		planID, data, toMillis(s.now()))
	return err
}

// DNSCheck returns a plan's latest DNS check, or ErrNotFound.
func (s *Store) DNSCheck(ctx context.Context, planID string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM dns_checks WHERE plan_id = ?", planID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
