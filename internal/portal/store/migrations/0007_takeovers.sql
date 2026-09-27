-- Taking over existing zrepl setups (see docs/design/replication.md, 5).
CREATE TABLE takeovers (
    plan_id      TEXT PRIMARY KEY REFERENCES plans (id) ON DELETE CASCADE,
    -- Protobuf-encoded ezdr.portal.v1.Takeover: preflight and progress.
    data         BLOB NOT NULL,
    -- Name of the hosts' main zrepl.yml backups.
    backup       TEXT NOT NULL DEFAULT '',
    -- Whether the plan's jobs are part of each host's desired state yet.
    primary_jobs INTEGER NOT NULL DEFAULT 0,
    dr_jobs      INTEGER NOT NULL DEFAULT 0,
    -- The step to run next when the takeover is running.
    next_step    INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL
);
