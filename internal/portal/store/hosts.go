package store

import (
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
}

const hostSelect = "SELECT h.id, h.hostname, h.machine_id, h.pve_version, h.client_version, " +
	"h.wireguard_public_key, h.tunnel_address, h.enrolled_at, h.last_seen_at, " +
	"EXISTS (SELECT 1 FROM hosts o WHERE o.machine_id = h.machine_id AND o.id != h.id), " +
	"i.host_id IS NOT NULL, coalesce(i.guest_count, 0), coalesce(i.guests_not_ready, 0), " +
	"(SELECT count(*) FROM plans p WHERE p.primary_host_id = h.id OR p.dr_host_id = h.id) " +
	"FROM hosts h LEFT JOIN host_inventory i ON i.host_id = h.id"

func scanHost(row interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var addr string
	var enrolled int64
	var lastSeen sql.NullInt64
	err := row.Scan(&h.ID, &h.Hostname, &h.MachineID, &h.PVEVersion, &h.ClientVersion,
		&h.WireGuardPublicKey, &addr, &enrolled, &lastSeen, &h.DuplicateMachineID,
		&h.HasInventory, &h.GuestCount, &h.GuestsNotReady, &h.PlanCount)
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
