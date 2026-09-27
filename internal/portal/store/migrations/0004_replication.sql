-- Plan lifecycle and per-host zrepl state (see docs/design/replication.md).
ALTER TABLE plans ADD COLUMN state TEXT NOT NULL DEFAULT 'draft';
-- Protobuf-encoded ezdr.plan.v1.PlanSpec applied to the hosts.
ALTER TABLE plans ADD COLUMN applied_spec BLOB;
ALTER TABLE plans ADD COLUMN applied_at INTEGER;

ALTER TABLE hosts ADD COLUMN zrepl_certificate TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN zrepl_version TEXT NOT NULL DEFAULT '';
-- Generation of the host's desired state; bumped when its content changes.
ALTER TABLE hosts ADD COLUMN desired_generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE hosts ADD COLUMN desired_hash BLOB;
-- Newest generation the host has handled, and why applying it failed.
ALTER TABLE hosts ADD COLUMN applied_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hosts ADD COLUMN apply_error TEXT NOT NULL DEFAULT '';
