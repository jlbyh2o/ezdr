-- Protected guests' Proxmox configurations, as reported by their primary
-- (see docs/design/test-failover.md, 2).
CREATE TABLE guest_configs (
    host_id     TEXT NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    vmid        INTEGER NOT NULL,
    type        TEXT NOT NULL,
    config      TEXT NOT NULL,
    reported_at INTEGER NOT NULL,
    PRIMARY KEY (host_id, vmid)
);
