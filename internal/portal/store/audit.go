package store

import (
	"context"
	"time"
)

// AuditEvent is an entry in the audit log.
type AuditEvent struct {
	Time          time.Time
	Actor         string
	Action        string
	Target        string
	SourceAddress string
	Detail        string
}

// AddAuditEvent appends an entry to the audit log. Time defaults to now.
func (s *Store) AddAuditEvent(ctx context.Context, e AuditEvent) error {
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO audit_log (time, actor, action, target, source_address, detail) VALUES (?, ?, ?, ?, ?, ?)",
		toMillis(e.Time), e.Actor, e.Action, e.Target, e.SourceAddress, e.Detail)
	return err
}

// ListAuditEvents returns up to limit entries, newest first.
func (s *Store) ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT time, actor, action, target, source_address, detail FROM audit_log ORDER BY id DESC LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var t int64
		if err := rows.Scan(&t, &e.Actor, &e.Action, &e.Target, &e.SourceAddress, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = fromMillis(t)
		events = append(events, e)
	}
	return events, rows.Err()
}
