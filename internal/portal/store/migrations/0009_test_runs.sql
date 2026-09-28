-- Test failovers (see docs/design/test-failover.md).
CREATE TABLE test_runs (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    -- Whether the test still needs the runner (starting, running, ending).
    active     INTEGER NOT NULL,
    -- Protobuf-encoded ezdr.portal.v1.TestRun.
    data       BLOB NOT NULL,
    -- Protobuf-encoded ezdr.client.v1.TestPrepare: the guests and disks,
    -- also needed for cleanup.
    prepare    BLOB NOT NULL,
    -- The step to run next.
    next_step  INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL
);

CREATE INDEX test_runs_plan ON test_runs (plan_id, started_at);
