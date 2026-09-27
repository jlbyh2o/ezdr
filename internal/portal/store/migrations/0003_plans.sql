-- DR plans (see docs/design/dr-plans.md).
CREATE TABLE plans (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE COLLATE NOCASE,
    -- Protobuf-encoded ezdr.plan.v1.PlanSpec.
    spec            BLOB NOT NULL,
    primary_host_id TEXT NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    dr_host_id      TEXT NOT NULL REFERENCES hosts (id) ON DELETE CASCADE,
    created_by      TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- Enforces that a guest belongs to at most one plan.
CREATE TABLE plan_guests (
    plan_id         TEXT NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    primary_host_id TEXT NOT NULL,
    vmid            INTEGER NOT NULL,
    PRIMARY KEY (primary_host_id, vmid)
);

CREATE INDEX plan_guests_plan_id ON plan_guests (plan_id);
