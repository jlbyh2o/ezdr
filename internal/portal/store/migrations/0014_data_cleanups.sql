-- Cleanups of replicated data (docs/design/cleanup.md): deleting a plan's
-- data before the plan, and removing what a takeover's old jobs left. Same
-- columns as failovers.
CREATE TABLE plan_deletions (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    active     INTEGER NOT NULL,        -- Whether the cleanup still needs the runner.
    data       BLOB NOT NULL,           -- Protobuf-encoded ezdr.portal.v1.DataCleanup.
    next_step  INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL
);
CREATE INDEX plan_deletions_plan ON plan_deletions (plan_id, started_at);

CREATE TABLE takeover_cleanups (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    active     INTEGER NOT NULL,
    data       BLOB NOT NULL,
    next_step  INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL
);
CREATE INDEX takeover_cleanups_plan ON takeover_cleanups (plan_id, started_at);
