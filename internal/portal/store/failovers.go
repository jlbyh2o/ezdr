package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FailoverRow is a stored failover.
type FailoverRow struct {
	ID     string
	PlanID string
	// Active is set while the failover needs the runner.
	Active bool
	// Data is the protobuf-encoded ezdr.portal.v1.Failover.
	Data      []byte
	NextStep  int
	StartedAt time.Time
}

const failoverSelect = "SELECT id, plan_id, active, data, next_step, started_at FROM failovers"

func scanFailovers(rows *sql.Rows) ([]FailoverRow, error) {
	defer func() { _ = rows.Close() }()
	var out []FailoverRow
	for rows.Next() {
		var f FailoverRow
		var started int64
		if err := rows.Scan(&f.ID, &f.PlanID, &f.Active, &f.Data, &f.NextStep, &started); err != nil {
			return nil, err
		}
		f.StartedAt = fromMillis(started)
		out = append(out, f)
	}
	return out, rows.Err()
}

// LatestFailover returns a plan's newest failover, or ErrNotFound.
func (s *Store) LatestFailover(ctx context.Context, planID string) (FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, failoverSelect+" WHERE plan_id = ? ORDER BY started_at DESC LIMIT 1", planID)
	if err != nil {
		return FailoverRow{}, err
	}
	fs, err := scanFailovers(rows)
	if err != nil {
		return FailoverRow{}, err
	}
	if len(fs) == 0 {
		return FailoverRow{}, ErrNotFound
	}
	return fs[0], nil
}

// ActiveFailovers returns the failovers that still need the runner.
func (s *Store) ActiveFailovers(ctx context.Context) ([]FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, failoverSelect+" WHERE active = 1")
	if err != nil {
		return nil, err
	}
	return scanFailovers(rows)
}

// ErrFailoverActive is returned when a plan already has a failover running.
var ErrFailoverActive = errors.New("the plan already has a failover running")

// CreateFailover stores a new failover, unless the plan has an active one.
func (s *Store) CreateFailover(ctx context.Context, f FailoverRow) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM failovers WHERE plan_id = ? AND active = 1", f.PlanID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrFailoverActive
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO failovers (id, plan_id, active, data, next_step, started_at) VALUES (?, ?, ?, ?, ?, ?)",
			f.ID, f.PlanID, f.Active, f.Data, f.NextStep, toMillis(f.StartedAt))
		return err
	})
}

// UpdateFailover saves a failover's progress.
func (s *Store) UpdateFailover(ctx context.Context, f FailoverRow) error {
	_, err := s.db.ExecContext(ctx, "UPDATE failovers SET active = ?, data = ?, next_step = ? WHERE id = ?",
		f.Active, f.Data, f.NextStep, f.ID)
	return err
}
