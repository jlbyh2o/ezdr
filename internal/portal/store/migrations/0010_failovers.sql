-- Failovers (see docs/design/failover.md).
CREATE TABLE failovers (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    -- Whether the failover still needs the runner.
    active     INTEGER NOT NULL,
    -- Protobuf-encoded ezdr.portal.v1.Failover.
    data       BLOB NOT NULL,
    next_step  INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL
);

CREATE INDEX failovers_plan ON failovers (plan_id, started_at);
