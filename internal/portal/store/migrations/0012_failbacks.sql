-- Failbacks (see docs/design/failback.md).
CREATE TABLE failbacks (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    -- Whether the failback still needs the runner.
    active     INTEGER NOT NULL,
    -- Protobuf-encoded ezdr.portal.v1.Failback.
    data       BLOB NOT NULL,
    next_step  INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL
);

CREATE INDEX failbacks_plan ON failbacks (plan_id, started_at);
