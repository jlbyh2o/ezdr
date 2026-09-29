-- Guests the user chose not to protect ("unprotected" in the UI): plans
-- don't warn about them (see docs/design/ui.md).
CREATE TABLE guest_exclusions (
    host_id     TEXT NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    vmid        INTEGER NOT NULL,
    excluded_by TEXT NOT NULL,
    excluded_at INTEGER NOT NULL,
    PRIMARY KEY (host_id, vmid)
);
