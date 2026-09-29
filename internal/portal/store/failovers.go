package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FailoverRow is a stored failover or failback.
type FailoverRow struct {
	ID     string
	PlanID string
	// Active is set while the failover needs the runner.
	Active bool
	// Data is the protobuf-encoded ezdr.portal.v1.Failover (or Failback).
	Data      []byte
	NextStep  int
	StartedAt time.Time
}

// The failovers and failbacks tables have the same columns.
const (
	failovers = "failovers"
	failbacks = "failbacks"
)

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

func (s *Store) latest(ctx context.Context, table, planID string) (FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, plan_id, active, data, next_step, started_at FROM "+table+ //nolint:gosec // fixed table names
		" WHERE plan_id = ? ORDER BY started_at DESC LIMIT 1", planID)
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

// list returns a plan's failovers (or failbacks), newest first.
func (s *Store) list(ctx context.Context, table, planID string) ([]FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, plan_id, active, data, next_step, started_at FROM "+table+ //nolint:gosec // fixed table names
		" WHERE plan_id = ? ORDER BY started_at DESC", planID)
	if err != nil {
		return nil, err
	}
	return scanFailovers(rows)
}

// byID returns one failover (or failback), or ErrNotFound.
func (s *Store) byID(ctx context.Context, table, id string) (FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, plan_id, active, data, next_step, started_at FROM "+table+" WHERE id = ?", id) //nolint:gosec // fixed table names
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

func (s *Store) active(ctx context.Context, table string) ([]FailoverRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, plan_id, active, data, next_step, started_at FROM "+table+" WHERE active = 1") //nolint:gosec // fixed table names
	if err != nil {
		return nil, err
	}
	return scanFailovers(rows)
}

func (s *Store) create(ctx context.Context, table string, f FailoverRow, errActive error) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE plan_id = ? AND active = 1", f.PlanID).Scan(&n); err != nil { //nolint:gosec // fixed table names
			return err
		}
		if n > 0 {
			return errActive
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO "+table+" (id, plan_id, active, data, next_step, started_at) VALUES (?, ?, ?, ?, ?, ?)", //nolint:gosec // fixed table names
			f.ID, f.PlanID, f.Active, f.Data, f.NextStep, toMillis(f.StartedAt))
		return err
	})
}

func (s *Store) update(ctx context.Context, table string, f FailoverRow) error {
	_, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET active = ?, data = ?, next_step = ? WHERE id = ?", //nolint:gosec // fixed table names
		f.Active, f.Data, f.NextStep, f.ID)
	return err
}

// LatestFailover returns a plan's newest failover, or ErrNotFound.
func (s *Store) LatestFailover(ctx context.Context, planID string) (FailoverRow, error) {
	return s.latest(ctx, failovers, planID)
}

// Failovers returns a plan's failovers, newest first.
func (s *Store) Failovers(ctx context.Context, planID string) ([]FailoverRow, error) {
	return s.list(ctx, failovers, planID)
}

// FailoverByID returns a failover, or ErrNotFound.
func (s *Store) FailoverByID(ctx context.Context, id string) (FailoverRow, error) {
	return s.byID(ctx, failovers, id)
}

// ActiveFailovers returns the failovers that still need the runner.
func (s *Store) ActiveFailovers(ctx context.Context) ([]FailoverRow, error) {
	return s.active(ctx, failovers)
}

// ErrFailoverActive is returned when a plan already has a failover running.
var ErrFailoverActive = errors.New("the plan already has a failover running")

// CreateFailover stores a new failover, unless the plan has an active one.
func (s *Store) CreateFailover(ctx context.Context, f FailoverRow) error {
	return s.create(ctx, failovers, f, ErrFailoverActive)
}

// UpdateFailover saves a failover's progress.
func (s *Store) UpdateFailover(ctx context.Context, f FailoverRow) error {
	return s.update(ctx, failovers, f)
}

// LatestFailback returns a plan's newest failback, or ErrNotFound.
func (s *Store) LatestFailback(ctx context.Context, planID string) (FailoverRow, error) {
	return s.latest(ctx, failbacks, planID)
}

// Failbacks returns a plan's failbacks, newest first.
func (s *Store) Failbacks(ctx context.Context, planID string) ([]FailoverRow, error) {
	return s.list(ctx, failbacks, planID)
}

// FailbackByID returns a failback, or ErrNotFound.
func (s *Store) FailbackByID(ctx context.Context, id string) (FailoverRow, error) {
	return s.byID(ctx, failbacks, id)
}

// ActiveFailbacks returns the failbacks that still need the runner.
func (s *Store) ActiveFailbacks(ctx context.Context) ([]FailoverRow, error) {
	return s.active(ctx, failbacks)
}

// ErrFailbackActive is returned when a plan already has a failback running.
var ErrFailbackActive = errors.New("the plan already has a failback running")

// CreateFailback stores a new failback, unless the plan has an active one.
func (s *Store) CreateFailback(ctx context.Context, f FailoverRow) error {
	return s.create(ctx, failbacks, f, ErrFailbackActive)
}

// UpdateFailback saves a failback's progress.
func (s *Store) UpdateFailback(ctx context.Context, f FailoverRow) error {
	return s.update(ctx, failbacks, f)
}

// Cleanup kinds, each stored in its own table (with the same columns as
// failovers).
const (
	CleanupPlan     = "plan_deletions"
	CleanupTakeover = "takeover_cleanups"
)

// LatestCleanup returns a plan's newest cleanup of a kind, or ErrNotFound.
func (s *Store) LatestCleanup(ctx context.Context, kind, planID string) (FailoverRow, error) {
	return s.latest(ctx, kind, planID)
}

// ActiveCleanups returns the cleanups of a kind that still need the runner.
func (s *Store) ActiveCleanups(ctx context.Context, kind string) ([]FailoverRow, error) {
	return s.active(ctx, kind)
}

// ErrCleanupActive is returned when a plan already has a cleanup running.
var ErrCleanupActive = errors.New("the plan already has a cleanup running")

// CreateCleanup stores a new cleanup, unless the plan has an active one of
// the same kind.
func (s *Store) CreateCleanup(ctx context.Context, kind string, f FailoverRow) error {
	return s.create(ctx, kind, f, ErrCleanupActive)
}

// UpdateCleanup saves a cleanup's progress.
func (s *Store) UpdateCleanup(ctx context.Context, kind string, f FailoverRow) error {
	return s.update(ctx, kind, f)
}
