-- Host-to-host replication tunnel (see docs/design/replication.md, 4.2).
ALTER TABLE hosts ADD COLUMN site_address TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN site_public_key BLOB;
