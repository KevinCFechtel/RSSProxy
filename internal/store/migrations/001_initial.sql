CREATE TABLE rssproxy_feeds (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    config JSONB NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE rssproxy_fetch_state (
    feed_id BIGINT PRIMARY KEY REFERENCES rssproxy_feeds(id) ON DELETE CASCADE,
    document JSONB,
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    last_attempt TIMESTAMPTZ,
    last_success TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT ''
);

-- Only identities survive disappearance from the source, not complete items.
CREATE TABLE rssproxy_identities (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed_id BIGINT NOT NULL REFERENCES rssproxy_feeds(id) ON DELETE CASCADE,
    key_hash TEXT NOT NULL,
    identity_key TEXT NOT NULL,
    proxy_guid TEXT NOT NULL UNIQUE,
    first_published TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (feed_id, key_hash),
    UNIQUE (feed_id, id)
);

CREATE TABLE rssproxy_aliases (
    feed_id BIGINT NOT NULL,
    rule_hash TEXT NOT NULL,
    kind TEXT NOT NULL,
    value_hash TEXT NOT NULL,
    identity_id BIGINT NOT NULL,
    PRIMARY KEY (feed_id, rule_hash, kind, value_hash, identity_id),
    FOREIGN KEY (feed_id, identity_id)
        REFERENCES rssproxy_identities(feed_id, id) ON DELETE CASCADE
);
CREATE INDEX rssproxy_alias_lookup ON rssproxy_aliases(feed_id, kind, value_hash);

CREATE TABLE rssproxy_imports (
    source TEXT PRIMARY KEY,
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    feed_count INTEGER NOT NULL
);
