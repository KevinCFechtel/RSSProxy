package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/jackc/pgx/v5"
)

// ImportLegacy automatically detects the old table in the target database, or
// reads an explicitly configured separate source. The source transaction is
// always read-only. A target-side marker makes the import one-time/idempotent.
func (s *Store) ImportLegacy(ctx context.Context, sourceURL string) (int, error) {
	target, err := s.Pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer target.Release()
	if _, err := target.Exec(ctx, `SELECT pg_advisory_lock(-912345679)`); err != nil {
		closeConnection(target)
		return 0, err
	}
	defer unlock(target, -912345679)
	var imported bool
	if err := target.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rssproxy_imports WHERE source='goRssDedup/v1')`).Scan(&imported); err != nil || imported {
		return 0, err
	}
	var source *pgx.Conn
	if sourceURL != "" {
		source, err = pgx.Connect(ctx, sourceURL)
		if err != nil {
			return 0, errors.New("legacy database connection failed")
		}
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = source.Close(closeCtx)
		}()
	} else {
		source = target.Conn()
	}
	readTx, err := source.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, err
	}
	defer func() { _ = readTx.Rollback(context.Background()) }()
	var exists bool
	if err := readTx.QueryRow(ctx, `SELECT to_regclass('rssdedupconfig') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		if sourceURL != "" {
			return 0, errors.New("configured legacy database does not contain rssDedupConfig")
		}
		return 0, nil
	}
	rows, err := readTx.Query(ctx, `SELECT id, COALESCE(httpEndpoint,''), COALESCE(feedName,''),
		COALESCE(feedURL,''), COALESCE(feedIDFromStartOrEnd,''), COALESCE(feedIDLength,0),
		COALESCE(feedIDFromStartOrEndLength,0), COALESCE(feedIconURL,''), COALESCE(artikelImageTag,'')
		FROM rssDedupConfig ORDER BY id`)
	if err != nil {
		return 0, errors.New("reading legacy configuration failed")
	}
	var configs []feed.Config
	for rows.Next() {
		c := feed.DefaultConfig()
		var oldID int64
		var icon, imageTag string
		c.Identity.Kind = "legacy"
		if err := rows.Scan(&oldID, &c.Path, &c.Name, &c.URL,
			&c.Identity.LegacyDirection, &c.Identity.LegacyLength,
			&c.Identity.LegacyOffset, &icon, &imageTag); err != nil {
			rows.Close()
			return 0, err
		}
		if icon != "" && icon != "Existing" {
			c.Overrides.ImageURL = feed.Override{Mode: "replace", Value: icon}
		}
		if strings.TrimSpace(imageTag) != "" {
			c.Overrides.ItemImageSource = imageTag
		}
		if err := c.Validate(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("legacy configuration %d needs correction: %w", oldID, err)
		}
		configs = append(configs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := readTx.Commit(ctx); err != nil {
		return 0, err
	}
	tx, err := target.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	count := 0
	for _, c := range configs {
		raw, err := json.Marshal(c)
		if err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO rssproxy_feeds(path,config) VALUES($1,$2)
			ON CONFLICT(path) DO NOTHING`, c.Path, raw)
		if err != nil {
			return 0, err
		}
		count += int(tag.RowsAffected())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rssproxy_imports(source,feed_count) VALUES('goRssDedup/v1',$1)`, count); err != nil {
		return 0, err
	}
	return count, tx.Commit(ctx)
}
