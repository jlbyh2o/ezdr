-- Latest replication status per host (see docs/design/replication.md).
CREATE TABLE host_replication (
    host_id     TEXT PRIMARY KEY REFERENCES hosts (id) ON DELETE CASCADE,
    -- Protobuf-encoded ezdr.client.v1.ReportReplicationRequest.
    status      BLOB NOT NULL,
    received_at INTEGER NOT NULL
);

-- Alerts, firing and resolved. key identifies the condition, such as
-- "rpo:<plan ID>".
CREATE TABLE alerts (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    key              TEXT NOT NULL,
    severity         TEXT NOT NULL,
    title            TEXT NOT NULL,
    message          TEXT NOT NULL,
    plan_id          TEXT NOT NULL,
    host_id          TEXT NOT NULL,
    fired_at         INTEGER NOT NULL,
    resolved_at      INTEGER,
    last_notified_at INTEGER NOT NULL
);

-- At most one firing alert per condition.
CREATE UNIQUE INDEX alerts_firing_key ON alerts (key) WHERE resolved_at IS NULL;
