package store

import (
	"context"
	"time"
)

// GuestExclusion records that a user chose not to protect a guest.
type GuestExclusion struct {
	By string
	At time.Time
}

// GuestExclusions returns the guests on hostID that the user chose not to
// protect, by VMID.
func (s *Store) GuestExclusions(ctx context.Context, hostID string) (map[uint32]GuestExclusion, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT vmid, excluded_by, excluded_at FROM guest_exclusions WHERE host_id = ?", hostID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]GuestExclusion{}
	for rows.Next() {
		var vmid uint32
		var e GuestExclusion
		var at int64
		if err := rows.Scan(&vmid, &e.By, &at); err != nil {
			return nil, err
		}
		e.At = fromMillis(at)
		out[vmid] = e
	}
	return out, rows.Err()
}

// SetGuestExcluded records (or, with excluded false, clears) the choice not
// to protect a guest.
func (s *Store) SetGuestExcluded(ctx context.Context, hostID string, vmid uint32, excluded bool, by string) error {
	if !excluded {
		_, err := s.db.ExecContext(ctx, "DELETE FROM guest_exclusions WHERE host_id = ? AND vmid = ?", hostID, vmid)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO guest_exclusions (host_id, vmid, excluded_by, excluded_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (host_id, vmid) DO NOTHING`, hostID, vmid, by, toMillis(s.now()))
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}
