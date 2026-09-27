package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

// Inventory is a host's latest reported inventory.
type Inventory struct {
	HostID string
	// Data is the protobuf-encoded ezdr.inventory.v1.Inventory.
	Data           []byte
	Hash           []byte
	CollectedAt    time.Time
	ChangedAt      time.Time
	ReceivedAt     time.Time
	GuestCount     int
	GuestsNotReady int
}

// PutInventory stores a host's inventory. ChangedAt is updated only when the
// hash differs from the stored one. It reports whether the inventory changed.
func (s *Store) PutInventory(ctx context.Context, inv Inventory) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		var oldHash []byte
		err := tx.QueryRowContext(ctx, "SELECT hash FROM host_inventory WHERE host_id = ?", inv.HostID).Scan(&oldHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		changed = !bytes.Equal(oldHash, inv.Hash)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO host_inventory (host_id, inventory, hash, collected_at, changed_at, received_at,
				guest_count, guests_not_ready)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (host_id) DO UPDATE SET
				inventory = excluded.inventory,
				hash = excluded.hash,
				collected_at = excluded.collected_at,
				changed_at = CASE WHEN host_inventory.hash = excluded.hash
					THEN host_inventory.changed_at ELSE excluded.changed_at END,
				received_at = excluded.received_at,
				guest_count = excluded.guest_count,
				guests_not_ready = excluded.guests_not_ready`,
			inv.HostID, inv.Data, inv.Hash, toMillis(inv.CollectedAt), toMillis(now), toMillis(now),
			inv.GuestCount, inv.GuestsNotReady)
		return err
	})
	return changed, err
}

// HostInventory returns a host's latest inventory.
func (s *Store) HostInventory(ctx context.Context, hostID string) (Inventory, error) {
	inv := Inventory{HostID: hostID}
	var collected, changed, received int64
	err := s.db.QueryRowContext(ctx, `
		SELECT inventory, hash, collected_at, changed_at, received_at, guest_count, guests_not_ready
		FROM host_inventory WHERE host_id = ?`, hostID).
		Scan(&inv.Data, &inv.Hash, &collected, &changed, &received, &inv.GuestCount, &inv.GuestsNotReady)
	if errors.Is(err, sql.ErrNoRows) {
		return Inventory{}, ErrNotFound
	}
	if err != nil {
		return Inventory{}, err
	}
	inv.CollectedAt = fromMillis(collected)
	inv.ChangedAt = fromMillis(changed)
	inv.ReceivedAt = fromMillis(received)
	return inv, nil
}
