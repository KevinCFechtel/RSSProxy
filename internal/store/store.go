package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/fetch"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mmcdole/gofeed"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound     = errors.New("feed not found")
	ErrConflict     = errors.New("endpoint already exists or configuration was changed by another request")
	ErrInvalidFeed  = errors.New("invalid source feed or identity rule")
	ErrInvalidGroup = errors.New("group sources must be existing regular feeds")
)

type Store struct{ Pool *pgxpool.Pool }

type State struct {
	Document    *feed.Document
	Validators  fetch.Validators
	LastAttempt *time.Time
	LastSuccess *time.Time
	LastError   string
}

type Summary struct {
	Config      feed.Config
	State       State
	SourceNames []string
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	config.MaxConns = 20
	config.MaxConnIdleTime = 5 * time.Minute
	config.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("create database pool failed")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("database connection failed")
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(-912345678)`); err != nil {
		closeConnection(conn)
		return err
	}
	defer unlock(conn, -912345678)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS rssproxy_migrations
		(version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rssproxy_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(sql)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO rssproxy_migrations(version) VALUES($1)`, name)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func decodeConfig(id int64, revision int64, raw []byte) (feed.Config, error) {
	var c feed.Config
	err := json.Unmarshal(raw, &c)
	c.ID, c.Revision = id, revision
	return c, err
}

func (s *Store) ByPath(ctx context.Context, path string) (feed.Config, error) {
	var id, rev int64
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT id, revision, config FROM rssproxy_feeds WHERE path=$1`, path).Scan(&id, &rev, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return feed.Config{}, ErrNotFound
	}
	if err != nil {
		return feed.Config{}, err
	}
	return decodeConfig(id, rev, raw)
}

func (s *Store) ByID(ctx context.Context, id int64) (feed.Config, error) {
	var rev int64
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT revision, config FROM rssproxy_feeds WHERE id=$1`, id).Scan(&rev, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return feed.Config{}, ErrNotFound
	}
	if err != nil {
		return feed.Config{}, err
	}
	return decodeConfig(id, rev, raw)
}

func (s *Store) List(ctx context.Context) ([]Summary, error) {
	rows, err := s.Pool.Query(ctx, `SELECT f.id, f.revision, f.config,
		s.last_attempt, s.last_success, COALESCE(s.last_error, '')
		FROM rssproxy_feeds f LEFT JOIN rssproxy_fetch_state s ON s.feed_id=f.id ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []Summary
	for rows.Next() {
		var id, rev int64
		var raw []byte
		var summary Summary
		if err := rows.Scan(&id, &rev, &raw, &summary.State.LastAttempt, &summary.State.LastSuccess, &summary.State.LastError); err != nil {
			return nil, err
		}
		summary.Config, err = decodeConfig(id, rev, raw)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(summaries))
	for _, summary := range summaries {
		names[summary.Config.ID] = summary.Config.Name
	}
	for i := range summaries {
		for _, id := range summaries[i].Config.SourceIDs {
			if name, ok := names[id]; ok {
				summaries[i].SourceNames = append(summaries[i].SourceNames, name)
			}
		}
	}
	return summaries, nil
}

// Locked owns a session advisory lock, not a transaction. Every request still
// performs its own source fetch; serialization prevents older responses from
// overwriting newer snapshots, including across multiple service instances.
type Locked struct {
	conn     *pgxpool.Conn
	Config   feed.Config
	borrowed bool
}

func (s *Store) Lock(ctx context.Context, id int64) (*Locked, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return lock(ctx, conn, id, false)
}

// LockSource borrows the group's connection, avoiding pool starvation when
// other requests occupy the pool while waiting for the group lock.
// The caller must close the source before using or closing the group again.
func (l *Locked) LockSource(ctx context.Context, id int64) (*Locked, error) {
	if l.Config.Type != "group" || !slices.Contains(l.Config.SourceIDs, id) || id == l.Config.ID {
		return nil, ErrInvalidGroup
	}
	return lock(ctx, l.conn, id, true)
}

func lock(ctx context.Context, conn *pgxpool.Conn, id int64, borrowed bool) (*Locked, error) {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, id); err != nil {
		closeConnection(conn) // cancellation must never leave a lock in the pool
		if !borrowed {
			conn.Release()
		}
		return nil, err
	}
	l := &Locked{conn: conn, Config: feed.Config{ID: id}, borrowed: borrowed}
	var raw []byte
	var rev int64
	err := conn.QueryRow(ctx, `SELECT revision, config FROM rssproxy_feeds WHERE id=$1`, id).Scan(&rev, &raw)
	if err != nil {
		l.Close()
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l.Config, err = decodeConfig(id, rev, raw)
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func closeConnection(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Conn().Close(ctx)
}

func unlock(conn *pgxpool.Conn, id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var released bool
	if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, id).Scan(&released); err != nil || !released {
		closeConnection(conn)
	}
}

func (l *Locked) Close() {
	unlock(l.conn, l.Config.ID)
	if !l.borrowed {
		l.conn.Release()
	}
}

func (l *Locked) State(ctx context.Context) (State, error) {
	var state State
	var raw []byte
	err := l.conn.QueryRow(ctx, `SELECT document, etag, last_modified, last_attempt, last_success, last_error
		FROM rssproxy_fetch_state WHERE feed_id=$1`, l.Config.ID).Scan(
		&raw, &state.Validators.ETag, &state.Validators.LastModified,
		&state.LastAttempt, &state.LastSuccess, &state.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(raw) > 0 {
		state.Document = &feed.Document{}
		if err := json.Unmarshal(raw, state.Document); err != nil {
			return state, err
		}
	}
	return state, nil
}

// StoreDocument records upstream validators independently of the source's own
// identity and presentation rules. Group and standalone requests share them.
func (l *Locked) StoreDocument(ctx context.Context, doc *feed.Document, validators fetch.Validators) error {
	if l.Config.Type != "" || doc == nil || doc.Feed == nil {
		return ErrInvalidGroup
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = l.conn.Exec(ctx, `INSERT INTO rssproxy_fetch_state
		(feed_id,document,etag,last_modified,last_attempt,last_success,last_error)
		VALUES($1,$2,$3,$4,now(),now(),'') ON CONFLICT(feed_id) DO UPDATE SET
		document=EXCLUDED.document,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,
		last_attempt=now(),last_success=now(),last_error=''`,
		l.Config.ID, raw, validators.ETag, validators.LastModified)
	return err
}

func (l *Locked) RecordError(ctx context.Context, message string) error {
	_, err := l.conn.Exec(ctx, `INSERT INTO rssproxy_fetch_state(feed_id,last_attempt,last_error)
		VALUES($1,now(),$2) ON CONFLICT(feed_id) DO UPDATE
		SET last_attempt=now(),last_error=EXCLUDED.last_error`, l.Config.ID, message)
	return err
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Commit renders before committing: bad items/overrides cannot replace a valid
// snapshot or persist half of a new identity batch.
func (l *Locked) Commit(ctx context.Context, doc *feed.Document, validators fetch.Validators) ([]byte, error) {
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	identities, err := resolveIdentities(ctx, tx, l.Config, doc)
	if err != nil {
		return nil, err
	}
	body, err := feed.Render(l.Config, doc, identities)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFeed, err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO rssproxy_fetch_state
		(feed_id,document,etag,last_modified,last_attempt,last_success,last_error)
		VALUES($1,$2,$3,$4,now(),now(),'') ON CONFLICT(feed_id) DO UPDATE SET
		document=EXCLUDED.document,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,
		last_attempt=now(),last_success=now(),last_error=''`,
		l.Config.ID, raw, validators.ETag, validators.LastModified)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return body, nil
}

func resolveIdentities(ctx context.Context, tx pgx.Tx, c feed.Config, doc *feed.Document) (map[string]feed.Identity, error) {
	if doc == nil || doc.Feed == nil {
		return nil, fmt.Errorf("%w: missing source document", ErrInvalidFeed)
	}
	result := make(map[string]feed.Identity)
	ruleJSON, _ := json.Marshal(c.Identity)
	ruleHash := hash(string(ruleJSON))
	selected := make(map[string]*gofeed.Item)
	var order []string
	for index, item := range doc.Feed.Items {
		if item == nil {
			return nil, fmt.Errorf("%w: source has an empty item", ErrInvalidFeed)
		}
		key, err := c.Identity.Key(item.GUID, item.Link)
		if err != nil {
			return nil, fmt.Errorf("%w: item %d: %w", ErrInvalidFeed, index+1, err)
		}
		if _, exists := selected[key]; !exists {
			order = append(order, key)
		}
		selected[key] = item
	}
	for _, key := range order {
		item := selected[key]
		// GUID and provider timestamps are not rendered as mutable article
		// content. A title/link/content/media change is, and receives its own
		// persisted modification time even if the source has no updated field.
		canonical := *item
		canonical.GUID, canonical.Published, canonical.Updated = "", "", ""
		canonical.PublishedParsed, canonical.UpdatedParsed = nil, nil
		canonicalJSON, err := json.Marshal(canonical)
		if err != nil {
			return nil, err
		}
		sourceHash, previousHash := hash(string(canonicalJSON)), ""
		var id int64
		identity := feed.Identity{Key: key}
		err = tx.QueryRow(ctx, `SELECT id,proxy_guid,first_published,last_changed,source_hash FROM rssproxy_identities
			WHERE feed_id=$1 AND key_hash=$2`, c.ID, hash(key)).Scan(&id, &identity.GUID, &identity.Published, &identity.Updated, &previousHash)
		if errors.Is(err, pgx.ErrNoRows) {
			// Only bridge aliases after a rule change. Within an unchanged rule,
			// different GUIDs sharing a URL can intentionally be distinct items.
			var ids []int64
			err = tx.QueryRow(ctx, `SELECT array_agg(DISTINCT identity_id) FROM rssproxy_aliases
				WHERE feed_id=$1 AND rule_hash<>$2 AND
				((kind='guid' AND value_hash=$3) OR (kind='link' AND value_hash=$4))`,
				c.ID, ruleHash, hash(item.GUID), hash(item.Link)).Scan(&ids)
			if err != nil {
				return nil, err
			}
			if len(ids) > 1 {
				return nil, fmt.Errorf("%w: identity rule change produced ambiguous historical aliases; review the rule", ErrInvalidFeed)
			}
			if len(ids) == 1 {
				// Store the new key as an alias too; lookup below can recover it
				// on subsequent source URLs after a rule change.
				id = ids[0]
				err = tx.QueryRow(ctx, `SELECT proxy_guid,first_published,last_changed,source_hash FROM rssproxy_identities
					WHERE id=$1 AND feed_id=$2`, id, c.ID).Scan(&identity.GUID, &identity.Published, &identity.Updated, &previousHash)
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE rssproxy_identities SET key_hash=$1,identity_key=$2 WHERE id=$3`, hash(key), key, id)
				}
			} else {
				identity.GUID = fmt.Sprintf("urn:uuid:%s", newUUID())
				err = tx.QueryRow(ctx, `INSERT INTO rssproxy_identities(feed_id,key_hash,identity_key,proxy_guid,first_published)
					VALUES($1,$2,$3,$4,$5) RETURNING id,first_published`,
					c.ID, hash(key), key, identity.GUID, item.PublishedParsed).Scan(&id, &identity.Published)
			}
		}
		if err != nil {
			return nil, err
		}
		if previousHash == "" {
			identity.Updated = postgresTime(item.UpdatedParsed)
		} else if previousHash != sourceHash {
			changed := time.Now().UTC().Truncate(time.Microsecond)
			identity.Updated = &changed
		}
		if previousHash != sourceHash {
			if _, err := tx.Exec(ctx, `UPDATE rssproxy_identities SET source_hash=$1,last_changed=$2 WHERE id=$3`,
				sourceHash, identity.Updated, id); err != nil {
				return nil, err
			}
		}
		if identity.Published == nil && item.PublishedParsed != nil {
			identity.Published = postgresTime(item.PublishedParsed)
			if _, err := tx.Exec(ctx, `UPDATE rssproxy_identities SET first_published=$1 WHERE id=$2 AND first_published IS NULL`, identity.Published, id); err != nil {
				return nil, err
			}
		}
		for _, alias := range []struct{ kind, value string }{{"guid", item.GUID}, {"link", item.Link}} {
			if alias.value == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO rssproxy_aliases(feed_id,rule_hash,kind,value_hash,identity_id)
				VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, c.ID, ruleHash, alias.kind, hash(alias.value), id); err != nil {
				return nil, err
			}
		}
		result[key] = identity
	}
	return result, nil
}

func postgresTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC().Truncate(time.Microsecond)
	return &normalized
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read cannot fail in supported Go versions
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// Source types cannot change, and key-share locks prevent concurrent deletion.
func validateGroupSources(ctx context.Context, tx pgx.Tx, c feed.Config) error {
	if c.Type != "group" {
		return nil
	}
	for _, id := range c.SourceIDs {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT config FROM rssproxy_feeds WHERE id=$1 FOR KEY SHARE`, id).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidGroup
		}
		if err != nil {
			return err
		}
		var source feed.Config
		if err := json.Unmarshal(raw, &source); err != nil {
			return err
		}
		if source.Type != "" {
			return ErrInvalidGroup
		}
	}
	return nil
}

func saveGroupSources(ctx context.Context, tx pgx.Tx, groupID int64, ids []int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM rssproxy_group_sources WHERE group_id=$1`, groupID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO rssproxy_group_sources(group_id,feed_id) VALUES($1,$2)`, groupID, id); err != nil {
			return classify(err)
		}
	}
	return nil
}

func (s *Store) Save(ctx context.Context, c feed.Config) (int64, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}
	if c.ID == 0 {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := validateGroupSources(ctx, tx, c); err != nil {
			return 0, err
		}
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO rssproxy_feeds(path,config) VALUES($1,$2) RETURNING id`, c.Path, raw).Scan(&id)
		if err != nil {
			return 0, classify(err)
		}
		if c.Type == "group" {
			if err := saveGroupSources(ctx, tx, id, c.SourceIDs); err != nil {
				return 0, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, classify(err)
		}
		return id, nil
	}
	l, err := s.Lock(ctx, c.ID)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	if l.Config.Revision != c.Revision || l.Config.Type != c.Type {
		return 0, ErrConflict
	}
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := validateGroupSources(ctx, tx, c); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE rssproxy_feeds SET path=$1,config=$2,revision=revision+1,updated_at=now()
		WHERE id=$3`, c.Path, raw, c.ID)
	if err != nil {
		return 0, classify(err)
	}
	if c.Type == "group" {
		if err := saveGroupSources(ctx, tx, c.ID, c.SourceIDs); err != nil {
			return 0, err
		}
	}
	if l.Config.URL != c.URL || !slices.Equal(l.Config.SourceIDs, c.SourceIDs) {
		// Never reuse a snapshot or validators after changing sources.
		if _, err := tx.Exec(ctx, `DELETE FROM rssproxy_fetch_state WHERE feed_id=$1`, c.ID); err != nil {
			return 0, err
		}
	}
	return c.ID, classify(tx.Commit(ctx))
}

func (s *Store) Delete(ctx context.Context, id, revision int64) error {
	l, err := s.Lock(ctx, id)
	if err != nil {
		return err
	}
	defer l.Close()
	if l.Config.Revision != revision {
		return ErrConflict
	}
	_, err = l.conn.Exec(ctx, `DELETE FROM rssproxy_feeds WHERE id=$1`, id)
	return classify(err)
}

func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503") {
		return ErrConflict
	}
	return err
}
