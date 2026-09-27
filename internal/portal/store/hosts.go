package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"
)

// Host is an enrolled Proxmox VE host.
type Host struct {
	ID                 string
	Hostname           string
	MachineID          string
	PVEVersion         string
	ClientVersion      string
	WireGuardPublicKey []byte
	TunnelAddress      netip.Addr
	EnrolledAt         time.Time
	LastSeenAt         time.Time // zero if never seen
	// DuplicateMachineID is set when another host has the same machine ID.
	DuplicateMachineID bool
	// From the latest inventory, if any.
	HasInventory   bool
	GuestCount     int
	GuestsNotReady int
	// PlanCount is the number of plans using the host as primary or DR host.
	PlanCount int
	// zrepl state.
	ZreplCertificate  string
	ZreplVersion      string
	DesiredGeneration uint64
	AppliedGeneration uint64
	ApplyError        string
	// Site tunnel state.
	SiteAddress   netip.Addr // invalid until allocated
	SitePublicKey []byte
}

const hostSelect = "SELECT h.id, h.hostname, h.machine_id, h.pve_version, h.client_version, " +
	"h.wireguard_public_key, h.tunnel_address, h.enrolled_at, h.last_seen_at, " +
	"EXISTS (SELECT 1 FROM hosts o WHERE o.machine_id = h.machine_id AND o.id != h.id), " +
	"i.host_id IS NOT NULL, coalesce(i.guest_count, 0), coalesce(i.guests_not_ready, 0), " +
	"(SELECT count(*) FROM plans p WHERE p.primary_host_id = h.id OR p.dr_host_id = h.id), " +
	"h.zrepl_certificate, h.zrepl_version, h.desired_generation, h.applied_generation, h.apply_error, " +
	"h.site_address, h.site_public_key " +
	"FROM hosts h LEFT JOIN host_inventory i ON i.host_id = h.id"

func scanHost(row interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var addr string
	var enrolled int64
	var lastSeen sql.NullInt64
	var site string
	err := row.Scan(&h.ID, &h.Hostname, &h.MachineID, &h.PVEVersion, &h.ClientVersion,
		&h.WireGuardPublicKey, &addr, &enrolled, &lastSeen, &h.DuplicateMachineID,
		&h.HasInventory, &h.GuestCount, &h.GuestsNotReady, &h.PlanCount,
		&h.ZreplCertificate, &h.ZreplVersion, &h.DesiredGeneration, &h.AppliedGeneration, &h.ApplyError,
		&site, &h.SitePublicKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Host{}, ErrNotFound
		}
		return Host{}, err
	}
	if h.TunnelAddress, err = netip.ParseAddr(addr); err != nil {
		return Host{}, err
	}
	h.EnrolledAt = fromMillis(enrolled)
	h.LastSeenAt = nullableTime(lastSeen)
	if site != "" {
		if h.SiteAddress, err = netip.ParseAddr(site); err != nil {
			return Host{}, err
		}
	}
	return h, nil
}

// ListHosts returns all hosts ordered by hostname.
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, hostSelect+" ORDER BY h.hostname, h.enrolled_at")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hosts []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// HostByID looks up a host by ID.
func (s *Store) HostByID(ctx context.Context, id string) (Host, error) {
	return scanHost(s.db.QueryRowContext(ctx, hostSelect+" WHERE h.id = ?", id))
}

// HostByTunnelAddress looks up a host by its tunnel address.
func (s *Store) HostByTunnelAddress(ctx context.Context, addr netip.Addr) (Host, error) {
	return scanHost(s.db.QueryRowContext(ctx, hostSelect+" WHERE h.tunnel_address = ?", addr.String()))
}

// TouchHost records that a host was seen and its current client version.
func (s *Store) TouchHost(ctx context.Context, id, clientVersion string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE hosts SET last_seen_at = ?, client_version = ? WHERE id = ?",
		toMillis(s.now()), clientVersion, id)
	return err
}

// DeleteHost removes a host.
func (s *Store) DeleteHost(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM hosts WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHostZrepl records a host's zrepl certificate and version. It reports
// whether the certificate changed.
func (s *Store) SetHostZrepl(ctx context.Context, id, certificate, version string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE hosts SET zrepl_certificate = ?, zrepl_version = ? WHERE id = ? AND zrepl_certificate != ?",
		certificate, version, id, certificate)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		_, err = s.db.ExecContext(ctx, "UPDATE hosts SET zrepl_version = ? WHERE id = ?", version, id)
	}
	return n > 0, err
}

// SetHostApplied records the newest desired state generation a host handled.
func (s *Store) SetHostApplied(ctx context.Context, id string, generation uint64, applyError string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE hosts SET applied_generation = ?, apply_error = ? WHERE id = ?", generation, applyError, id)
	return err
}

// SetDesiredHash stores the hash of a host's desired state, bumping its
// generation when the hash changed. It returns the current generation.
func (s *Store) SetDesiredHash(ctx context.Context, id string, hash []byte) (uint64, error) {
	var gen uint64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var old []byte
		if err := tx.QueryRowContext(ctx, "SELECT desired_generation, desired_hash FROM hosts WHERE id = ?", id).
			Scan(&gen, &old); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if bytes.Equal(old, hash) {
			return nil
		}
		gen++
		_, err := tx.ExecContext(ctx, "UPDATE hosts SET desired_generation = ?, desired_hash = ? WHERE id = ?", gen, hash, id)
		return err
	})
	return gen, err
}

// SetSitePublicKey records a host's site tunnel public key. It reports
// whether the key changed.
func (s *Store) SetSitePublicKey(ctx context.Context, id string, key []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE hosts SET site_public_key = ? WHERE id = ? AND (site_public_key IS NULL OR site_public_key != ?)", key, id, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AllocateSiteAddress returns a host's site tunnel address, assigning the
// first free host address in prefix if it has none.
func (s *Store) AllocateSiteAddress(ctx context.Context, id string, prefix netip.Prefix) (netip.Addr, error) {
	var addr netip.Addr
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, "SELECT site_address FROM hosts WHERE id = ?", id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if cur != "" {
			a, err := netip.ParseAddr(cur)
			if err == nil && prefix.Contains(a) {
				addr = a
				return nil
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT site_address FROM hosts WHERE site_address != '' AND id != ?", id)
		if err != nil {
			return err
		}
		used := map[netip.Addr]bool{}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				_ = rows.Close()
				return err
			}
			if p, err := netip.ParseAddr(a); err == nil {
				used[p] = true
			}
		}
		_ = rows.Close()
		for a := prefix.Addr().Next(); prefix.Contains(a) && prefix.Contains(a.Next()); a = a.Next() {
			if !used[a] {
				addr = a
				_, err := tx.ExecContext(ctx, "UPDATE hosts SET site_address = ? WHERE id = ?", a.String(), id)
				return err
			}
		}
		return errors.New("no free site tunnel addresses; configure a larger EZDR_SITE_TUNNEL_PREFIX")
	})
	return addr, err
}
