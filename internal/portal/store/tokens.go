package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/netip"
	"time"
)

// ErrTokenInvalid is returned when an enrollment token does not exist, is
// expired, used, or revoked, or its secret does not match. The cases are not
// distinguished, so callers cannot probe token state.
var ErrTokenInvalid = errors.New("invalid enrollment token")

// Token is an enrollment token. The secret itself is never stored.
type Token struct {
	ID          string
	SecretHash  []byte
	Description string
	CreatedBy   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UsedAt      time.Time // zero if unused
	RevokedAt   time.Time // zero if not revoked
	HostID      string
}

const tokenSelect = "SELECT id, secret_hash, description, created_by, created_at, expires_at, " +
	"used_at, revoked_at, host_id FROM enrollment_tokens"

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var created, expires int64
	var used, revoked sql.NullInt64
	var hostID sql.NullString
	err := row.Scan(&t.ID, &t.SecretHash, &t.Description, &t.CreatedBy, &created, &expires,
		&used, &revoked, &hostID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Token{}, ErrNotFound
		}
		return Token{}, err
	}
	t.CreatedAt = fromMillis(created)
	t.ExpiresAt = fromMillis(expires)
	t.UsedAt = nullableTime(used)
	t.RevokedAt = nullableTime(revoked)
	t.HostID = hostID.String
	return t, nil
}

// CreateToken stores a new enrollment token. ID, SecretHash, Description,
// CreatedBy, and ExpiresAt must be set.
func (s *Store) CreateToken(ctx context.Context, t Token) (Token, error) {
	t.CreatedAt = s.now().UTC()
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO enrollment_tokens (id, secret_hash, description, created_by, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?)",
		t.ID, t.SecretHash, t.Description, t.CreatedBy, toMillis(t.CreatedAt), toMillis(t.ExpiresAt))
	return t, err
}

// ListTokens returns all tokens, newest first.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, tokenSelect+" ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tokens []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeToken revokes an unused, unrevoked token.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE enrollment_tokens SET revoked_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL",
		toMillis(s.now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// validToken returns the token if it is usable and secretHash matches.
func validToken(t Token, secretHash []byte, now time.Time) bool {
	return subtle.ConstantTimeCompare(t.SecretHash, secretHash) == 1 &&
		t.UsedAt.IsZero() && t.RevokedAt.IsZero() && now.Before(t.ExpiresAt)
}

// CheckToken reports whether a token is usable, without using it.
func (s *Store) CheckToken(ctx context.Context, id string, secretHash []byte) error {
	t, err := scanToken(s.db.QueryRowContext(ctx, tokenSelect+" WHERE id = ?", id))
	if errors.Is(err, ErrNotFound) || (err == nil && !validToken(t, secretHash, s.now())) {
		return ErrTokenInvalid
	}
	return err
}

// AddressAllocator picks a free tunnel address given those already in use.
type AddressAllocator func(used []netip.Addr) (netip.Addr, error)

// EnrollHost validates and uses a token, allocates a tunnel address, and
// creates the host, all in one transaction. ID, Hostname, MachineID,
// PVEVersion, ClientVersion, and WireGuardPublicKey must be set.
func (s *Store) EnrollHost(ctx context.Context, tokenID string, secretHash []byte, h Host, alloc AddressAllocator) (Host, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		t, err := scanToken(tx.QueryRowContext(ctx, tokenSelect+" WHERE id = ?", tokenID))
		if errors.Is(err, ErrNotFound) || (err == nil && !validToken(t, secretHash, now)) {
			return ErrTokenInvalid
		}
		if err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, "SELECT tunnel_address FROM hosts")
		if err != nil {
			return err
		}
		var used []netip.Addr
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				_ = rows.Close()
				return err
			}
			if addr, err := netip.ParseAddr(a); err == nil {
				used = append(used, addr)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if h.TunnelAddress, err = alloc(used); err != nil {
			return err
		}

		h.EnrolledAt = now.UTC()
		_, err = tx.ExecContext(ctx,
			"INSERT INTO hosts (id, hostname, machine_id, pve_version, client_version, "+
				"wireguard_public_key, tunnel_address, enrolled_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			h.ID, h.Hostname, h.MachineID, h.PVEVersion, h.ClientVersion,
			h.WireGuardPublicKey, h.TunnelAddress.String(), toMillis(h.EnrolledAt))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE enrollment_tokens SET used_at = ?, host_id = ? WHERE id = ?",
			toMillis(now), h.ID, tokenID)
		return err
	})
	if err != nil {
		return Host{}, err
	}
	return h, nil
}
