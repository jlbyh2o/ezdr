-- The latest DNS check of each plan's records (see docs/design/failover.md, 6).
CREATE TABLE dns_checks (
    plan_id    TEXT PRIMARY KEY REFERENCES plans (id) ON DELETE CASCADE,
    -- Protobuf-encoded ezdr.portal.v1.PlanDnsStatus.
    data       BLOB NOT NULL,
    checked_at INTEGER NOT NULL
);
