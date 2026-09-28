package store

import (
	"context"
	"database/sql"
	"time"
)

// GuestConfig is a guest's configuration as reported by its host.
type GuestConfig struct {
	VMID   uint32
	Type   string
	Config string
	// ChangedAt is when the host first reported this configuration.
	ChangedAt time.Time
}

// PutGuestConfigs replaces a host's reported guest configurations and
// reports whether any changed. Unchanged configurations keep their time, so
// periodic reports don't count as changes.
func (s *Store) PutGuestConfigs(ctx context.Context, hostID string, configs []GuestConfig) (bool, error) {
	changed := false
	now := toMillis(s.now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		type prev struct {
			value string
			at    int64
		}
		old := map[uint32]prev{}
		rows, err := tx.QueryContext(ctx, "SELECT vmid, type || ':' || config, reported_at FROM guest_configs WHERE host_id = ?", hostID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uint32
			var v prev
			if err := rows.Scan(&id, &v.value, &v.at); err != nil {
				_ = rows.Close()
				return err
			}
			old[id] = v
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(old) != len(configs) {
			changed = true
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM guest_configs WHERE host_id = ?", hostID); err != nil {
			return err
		}
		for _, c := range configs {
			at := now
			if v, ok := old[c.VMID]; ok && v.value == c.Type+":"+c.Config {
				at = v.at
			} else {
				changed = true
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO guest_configs (host_id, vmid, type, config, reported_at) VALUES (?, ?, ?, ?, ?)",
				hostID, c.VMID, c.Type, c.Config, at); err != nil {
				return err
			}
		}
		return nil
	})
	return changed, err
}

// GuestConfigs returns a host's reported guest configurations by VMID.
func (s *Store) GuestConfigs(ctx context.Context, hostID string) (map[uint32]GuestConfig, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT vmid, type, config, reported_at FROM guest_configs WHERE host_id = ?", hostID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]GuestConfig{}
	for rows.Next() {
		var c GuestConfig
		var at int64
		if err := rows.Scan(&c.VMID, &c.Type, &c.Config, &at); err != nil {
			return nil, err
		}
		c.ChangedAt = fromMillis(at)
		out[c.VMID] = c
	}
	return out, rows.Err()
}
