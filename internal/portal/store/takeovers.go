package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Takeover is a plan's takeover of an existing zrepl setup.
type Takeover struct {
	PlanID string
	// Data is the protobuf-encoded ezdr.portal.v1.Takeover.
	Data   []byte
	Backup string
	// PrimaryJobs and DRJobs say whether the plan's zrepl jobs are part of
	// each host's desired state yet.
	PrimaryJobs, DRJobs bool
	NextStep            int
	UpdatedAt           time.Time
}

// TakeoverByPlan returns a plan's takeover, or ErrNotFound.
func (s *Store) TakeoverByPlan(ctx context.Context, planID string) (Takeover, error) {
	t := Takeover{PlanID: planID}
	var updated int64
	err := s.db.QueryRowContext(ctx,
		"SELECT data, backup, primary_jobs, dr_jobs, next_step, updated_at FROM takeovers WHERE plan_id = ?", planID).
		Scan(&t.Data, &t.Backup, &t.PrimaryJobs, &t.DRJobs, &t.NextStep, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Takeover{}, ErrNotFound
	}
	t.UpdatedAt = fromMillis(updated)
	return t, err
}

// ListTakeovers returns every stored takeover.
func (s *Store) ListTakeovers(ctx context.Context) ([]Takeover, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT plan_id, data, backup, primary_jobs, dr_jobs, next_step, updated_at FROM takeovers")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Takeover
	for rows.Next() {
		var t Takeover
		var updated int64
		if err := rows.Scan(&t.PlanID, &t.Data, &t.Backup, &t.PrimaryJobs, &t.DRJobs, &t.NextStep, &updated); err != nil {
			return nil, err
		}
		t.UpdatedAt = fromMillis(updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutTakeover creates or replaces a plan's takeover.
func (s *Store) PutTakeover(ctx context.Context, t Takeover) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO takeovers (plan_id, data, backup, primary_jobs, dr_jobs, next_step, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (plan_id) DO UPDATE SET data = excluded.data, backup = excluded.backup,
			primary_jobs = excluded.primary_jobs, dr_jobs = excluded.dr_jobs, next_step = excluded.next_step,
			updated_at = excluded.updated_at`,
		t.PlanID, t.Data, t.Backup, t.PrimaryJobs, t.DRJobs, t.NextStep, toMillis(s.now()))
	return err
}

// DeleteTakeover removes a plan's takeover, if any.
func (s *Store) DeleteTakeover(ctx context.Context, planID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM takeovers WHERE plan_id = ?", planID)
	return err
}
