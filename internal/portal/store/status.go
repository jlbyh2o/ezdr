package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PutReplicationStatus stores a host's latest replication status.
func (s *Store) PutReplicationStatus(ctx context.Context, hostID string, status []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_replication (host_id, status, received_at) VALUES (?, ?, ?)
		ON CONFLICT (host_id) DO UPDATE SET status = excluded.status, received_at = excluded.received_at`,
		hostID, status, toMillis(s.now()))
	return err
}

// ReplicationStatus returns a host's latest replication status and when it
// was received.
func (s *Store) ReplicationStatus(ctx context.Context, hostID string) ([]byte, time.Time, error) {
	var b []byte
	var at int64
	err := s.db.QueryRowContext(ctx, "SELECT status, received_at FROM host_replication WHERE host_id = ?", hostID).Scan(&b, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	return b, fromMillis(at), err
}

// Alert is a firing or resolved alert.
type Alert struct {
	ID             int64
	Key            string
	Severity       string
	Title          string
	Message        string
	PlanID         string
	HostID         string
	FiredAt        time.Time
	ResolvedAt     time.Time // zero while firing
	LastNotifiedAt time.Time
}

const alertSelect = "SELECT id, key, severity, title, message, plan_id, host_id, fired_at, resolved_at, last_notified_at FROM alerts"

func (s *Store) queryAlerts(ctx context.Context, query string, args ...any) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		var a Alert
		var fired, notified int64
		var resolved sql.NullInt64
		if err := rows.Scan(&a.ID, &a.Key, &a.Severity, &a.Title, &a.Message, &a.PlanID, &a.HostID,
			&fired, &resolved, &notified); err != nil {
			return nil, err
		}
		a.FiredAt, a.ResolvedAt, a.LastNotifiedAt = fromMillis(fired), nullableTime(resolved), fromMillis(notified)
		out = append(out, a)
	}
	return out, rows.Err()
}

// FiringAlerts returns alerts that haven't resolved.
func (s *Store) FiringAlerts(ctx context.Context) ([]Alert, error) {
	return s.queryAlerts(ctx, alertSelect+" WHERE resolved_at IS NULL ORDER BY fired_at")
}

// ListAlerts returns up to limit alerts, firing first, then newest.
func (s *Store) ListAlerts(ctx context.Context, limit int) ([]Alert, error) {
	return s.queryAlerts(ctx, alertSelect+" ORDER BY resolved_at IS NOT NULL, fired_at DESC LIMIT ?", limit)
}

// FireAlert records a new firing alert. It fails with ErrConflict if one
// with the same key is already firing.
func (s *Store) FireAlert(ctx context.Context, a Alert) (Alert, error) {
	now := s.now()
	a.FiredAt, a.LastNotifiedAt = now, now
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO alerts (key, severity, title, message, plan_id, host_id, fired_at, last_notified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Key, a.Severity, a.Title, a.Message, a.PlanID, a.HostID, toMillis(now), toMillis(now))
	if isUniqueViolation(err) {
		return Alert{}, ErrConflict
	}
	if err != nil {
		return Alert{}, err
	}
	a.ID, _ = res.LastInsertId()
	return a, nil
}

// UpdateAlert updates a firing alert's message and, if notified, its last
// notification time.
func (s *Store) UpdateAlert(ctx context.Context, id int64, message string, notified bool) error {
	q := "UPDATE alerts SET message = ? WHERE id = ?"
	args := []any{message, id}
	if notified {
		q = "UPDATE alerts SET message = ?, last_notified_at = ? WHERE id = ?"
		args = []any{message, toMillis(s.now()), id}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// ResolveAlert marks a firing alert resolved.
func (s *Store) ResolveAlert(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE alerts SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL",
		toMillis(s.now()), id)
	return err
}
