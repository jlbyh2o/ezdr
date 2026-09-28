package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TestRun is a stored test failover.
type TestRun struct {
	ID     string
	PlanID string
	// Active is set while the test needs the runner.
	Active bool
	// Data is the protobuf-encoded ezdr.portal.v1.TestRun and Prepare the
	// ezdr.client.v1.TestPrepare.
	Data, Prepare []byte
	NextStep      int
	StartedAt     time.Time
}

const testRunSelect = "SELECT id, plan_id, active, data, prepare, next_step, started_at FROM test_runs"

func scanTestRuns(rows *sql.Rows) ([]TestRun, error) {
	defer func() { _ = rows.Close() }()
	var out []TestRun
	for rows.Next() {
		var t TestRun
		var started int64
		if err := rows.Scan(&t.ID, &t.PlanID, &t.Active, &t.Data, &t.Prepare, &t.NextStep, &started); err != nil {
			return nil, err
		}
		t.StartedAt = fromMillis(started)
		out = append(out, t)
	}
	return out, rows.Err()
}

// TestRunByID returns a test, or ErrNotFound.
func (s *Store) TestRunByID(ctx context.Context, id string) (TestRun, error) {
	rows, err := s.db.QueryContext(ctx, testRunSelect+" WHERE id = ?", id)
	if err != nil {
		return TestRun{}, err
	}
	runs, err := scanTestRuns(rows)
	if err != nil {
		return TestRun{}, err
	}
	if len(runs) == 0 {
		return TestRun{}, ErrNotFound
	}
	return runs[0], nil
}

// TestRunsByPlan returns a plan's tests, newest first.
func (s *Store) TestRunsByPlan(ctx context.Context, planID string, limit int) ([]TestRun, error) {
	rows, err := s.db.QueryContext(ctx, testRunSelect+" WHERE plan_id = ? ORDER BY started_at DESC LIMIT ?", planID, limit)
	if err != nil {
		return nil, err
	}
	return scanTestRuns(rows)
}

// ActiveTestRuns returns the tests that still need the runner.
func (s *Store) ActiveTestRuns(ctx context.Context) ([]TestRun, error) {
	rows, err := s.db.QueryContext(ctx, testRunSelect+" WHERE active = 1")
	if err != nil {
		return nil, err
	}
	return scanTestRuns(rows)
}

// ErrTestActive is returned when a plan already has a test that hasn't ended.
var ErrTestActive = errors.New("the plan already has a test failover that hasn't ended")

// CreateTestRun stores a new test, unless the plan has an active one.
func (s *Store) CreateTestRun(ctx context.Context, t TestRun) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM test_runs WHERE plan_id = ? AND active = 1", t.PlanID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrTestActive
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO test_runs (id, plan_id, active, data, prepare, next_step, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			t.ID, t.PlanID, t.Active, t.Data, t.Prepare, t.NextStep, toMillis(t.StartedAt))
		return err
	})
}

// UpdateTestRun saves a test's progress.
func (s *Store) UpdateTestRun(ctx context.Context, t TestRun) error {
	_, err := s.db.ExecContext(ctx, "UPDATE test_runs SET active = ?, data = ?, next_step = ? WHERE id = ?",
		t.Active, t.Data, t.NextStep, t.ID)
	return err
}
