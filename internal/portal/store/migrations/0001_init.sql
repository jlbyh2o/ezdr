-- Times are stored as Unix milliseconds (UTC).

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    -- Encrypted TOTP secret; NULL until TOTP setup is complete.
    totp_secret   BLOB,
    created_at    INTEGER NOT NULL
);

CREATE TABLE recovery_codes (
    user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash BLOB NOT NULL,
    used_at   INTEGER,
    PRIMARY KEY (user_id, code_hash)
);

CREATE TABLE sessions (
    id_hash      BLOB PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);

CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE hosts (
    id                   TEXT PRIMARY KEY,
    hostname             TEXT NOT NULL,
    machine_id           TEXT NOT NULL,
    pve_version          TEXT NOT NULL,
    client_version       TEXT NOT NULL,
    wireguard_public_key BLOB NOT NULL UNIQUE,
    tunnel_address       TEXT NOT NULL UNIQUE,
    enrolled_at          INTEGER NOT NULL,
    last_seen_at         INTEGER
);

CREATE TABLE enrollment_tokens (
    id          TEXT PRIMARY KEY,
    secret_hash BLOB NOT NULL,
    description TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    revoked_at  INTEGER,
    host_id     TEXT REFERENCES hosts (id) ON DELETE SET NULL
);

CREATE TABLE audit_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    time           INTEGER NOT NULL,
    actor          TEXT NOT NULL,
    action         TEXT NOT NULL,
    target         TEXT NOT NULL,
    source_address TEXT NOT NULL,
    detail         TEXT NOT NULL
);

-- Encrypted portal secrets, such as the portal's WireGuard private key.
CREATE TABLE secrets (
    name  TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
