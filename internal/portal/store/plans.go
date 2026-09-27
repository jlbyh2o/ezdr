package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrGuestInOtherPlan is returned when saving a plan that includes a guest
// another plan already protects.
var ErrGuestInOtherPlan = errors.New("a guest in this plan is already protected by another plan")

// Plan is a stored DR plan.
type Plan struct {
	ID string
	// Spec is the protobuf-encoded ezdr.plan.v1.PlanSpec.
	Spec          []byte
	Name          string
	PrimaryHostID string
	DRHostID      string
	VMIDs         []uint32
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SavePlan creates or replaces a plan. Name, PrimaryHostID, DRHostID, VMIDs,
// and Spec must be set; ID is generated for new plans (when empty).
func (s *Store) SavePlan(ctx context.Context, p Plan) (Plan, error) {
	now := s.now().UTC()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if p.ID == "" {
			p.ID, p.CreatedAt = NewID(), now
			_, err := tx.ExecContext(ctx, `
				INSERT INTO plans (id, name, spec, primary_host_id, dr_host_id, created_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				p.ID, p.Name, p.Spec, p.PrimaryHostID, p.DRHostID, p.CreatedBy, toMillis(now), toMillis(now))
			if err != nil {
				return planError(err)
			}
		} else {
			res, err := tx.ExecContext(ctx, `
				UPDATE plans SET name = ?, spec = ?, primary_host_id = ?, dr_host_id = ?, updated_at = ?
				WHERE id = ?`, p.Name, p.Spec, p.PrimaryHostID, p.DRHostID, toMillis(now), p.ID)
			if err != nil {
				return planError(err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrNotFound
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM plan_guests WHERE plan_id = ?", p.ID); err != nil {
			return err
		}
		for _, id := range p.VMIDs {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO plan_guests (plan_id, primary_host_id, vmid) VALUES (?, ?, ?)", p.ID, p.PrimaryHostID, id)
			if isUniqueViolation(err) {
				return ErrGuestInOtherPlan
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Plan{}, err
	}
	p.UpdatedAt = now
	return s.PlanByID(ctx, p.ID)
}

func planError(err error) error {
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil && isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

const planSelect = "SELECT id, name, spec, primary_host_id, dr_host_id, created_by, created_at, updated_at FROM plans"

func (s *Store) scanPlans(ctx context.Context, rows *sql.Rows) ([]Plan, error) {
	defer func() { _ = rows.Close() }()
	var plans []Plan
	for rows.Next() {
		var p Plan
		var created, updated int64
		if err := rows.Scan(&p.ID, &p.Name, &p.Spec, &p.PrimaryHostID, &p.DRHostID, &p.CreatedBy, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = fromMillis(created), fromMillis(updated)
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range plans {
		ids, err := s.planVMIDs(ctx, plans[i].ID)
		if err != nil {
			return nil, err
		}
		plans[i].VMIDs = ids
	}
	return plans, nil
}

func (s *Store) planVMIDs(ctx context.Context, planID string) ([]uint32, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT vmid FROM plan_guests WHERE plan_id = ? ORDER BY vmid", planID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []uint32
	for rows.Next() {
		var id uint32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListPlans returns all plans ordered by name.
func (s *Store) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, planSelect+" ORDER BY name")
	if err != nil {
		return nil, err
	}
	return s.scanPlans(ctx, rows)
}

// PlanByID looks up a plan.
func (s *Store) PlanByID(ctx context.Context, id string) (Plan, error) {
	rows, err := s.db.QueryContext(ctx, planSelect+" WHERE id = ?", id)
	if err != nil {
		return Plan{}, err
	}
	plans, err := s.scanPlans(ctx, rows)
	if err != nil {
		return Plan{}, err
	}
	if len(plans) == 0 {
		return Plan{}, ErrNotFound
	}
	return plans[0], nil
}

// DeletePlan removes a plan.
func (s *Store) DeletePlan(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM plans WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GuestPlans maps a primary host's protected VMIDs to their plan IDs.
func (s *Store) GuestPlans(ctx context.Context, primaryHostID string) (map[uint32]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT vmid, plan_id FROM plan_guests WHERE primary_host_id = ?", primaryHostID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[uint32]string{}
	for rows.Next() {
		var id uint32
		var plan string
		if err := rows.Scan(&id, &plan); err != nil {
			return nil, err
		}
		m[id] = plan
	}
	return m, rows.Err()
}
