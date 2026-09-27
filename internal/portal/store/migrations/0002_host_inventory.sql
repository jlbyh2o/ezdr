-- Latest inventory per host (see docs/design/inventory.md).
CREATE TABLE host_inventory (
    host_id          TEXT PRIMARY KEY REFERENCES hosts (id) ON DELETE CASCADE,
    -- Protobuf-encoded ezdr.inventory.v1.Inventory.
    inventory        BLOB NOT NULL,
    hash             BLOB NOT NULL,
    collected_at     INTEGER NOT NULL,
    changed_at       INTEGER NOT NULL,
    received_at      INTEGER NOT NULL,
    guest_count      INTEGER NOT NULL,
    guests_not_ready INTEGER NOT NULL
);
