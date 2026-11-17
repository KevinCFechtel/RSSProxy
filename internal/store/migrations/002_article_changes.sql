ALTER TABLE rssproxy_identities
    ADD COLUMN source_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_changed TIMESTAMPTZ;
