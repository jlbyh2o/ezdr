package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Secret returns a stored secret's (encrypted) value.
func (s *Store) Secret(ctx context.Context, name string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM secrets WHERE name = ?", name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// PutSecret stores a secret's (encrypted) value, replacing any existing one.
func (s *Store) PutSecret(ctx context.Context, name string, value []byte) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO secrets (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value",
		name, value)
	return err
}

// DeleteSecret removes a secret, if it exists.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE name = ?", name)
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
