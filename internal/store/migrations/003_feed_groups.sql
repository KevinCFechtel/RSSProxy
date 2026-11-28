-- Shared feed rows preserve endpoint uniqueness and article identity lifetimes.
CREATE TABLE rssproxy_group_sources (
    group_id BIGINT NOT NULL REFERENCES rssproxy_feeds(id) ON DELETE CASCADE,
    feed_id BIGINT NOT NULL REFERENCES rssproxy_feeds(id) ON DELETE RESTRICT,
    PRIMARY KEY (group_id, feed_id),
    CHECK (group_id <> feed_id)
);
CREATE INDEX rssproxy_group_sources_feed_idx ON rssproxy_group_sources(feed_id);
