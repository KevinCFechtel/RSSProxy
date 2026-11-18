package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/fetch"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
	"github.com/KevinCFechtel/RSSProxy/internal/testutil"
)

func configuration() feed.Config {
	c := feed.DefaultConfig()
	c.Name, c.Path, c.URL = "Example", "/feeds/example", "https://example.org/rss"
	c.Identity = feed.IdentityRule{Source: "guid", Kind: "regex", Regex: `-(?P<id>[0-9]+)\.html$`}
	return c
}

func document(t *testing.T, title, slug string) *feed.Document {
	t.Helper()
	doc, err := feed.Parse([]byte(`<rss version="2.0"><channel><title>Feed</title><link>https://example.org</link><description>News</description>
<item><title>` + title + `</title><guid>https://example.org/` + slug + `-123.html</guid><pubDate>Mon, 16 Nov 2026 12:00:00 +0000</pubDate></item></channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func commit(t *testing.T, db *store.Store, id int64, doc *feed.Document) []byte {
	t.Helper()
	l, err := db.Lock(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	body, err := l.Commit(context.Background(), doc, fetch.Validators{ETag: `"source"`})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestStableIdentitiesUpdatesAndRuleChanges(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	c := configuration()
	id, err := db.Save(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	first := commit(t, db, id, document(t, "Old", "old"))
	second := commit(t, db, id, document(t, "New", "new"))
	a, _ := feed.Parse(first)
	b, _ := feed.Parse(second)
	if a.Feed.Items[0].GUID != b.Feed.Items[0].GUID || b.Feed.Items[0].Title != "New" {
		t.Fatalf("article update produced a new GUID or stale title")
	}
	if !a.Feed.Items[0].PublishedParsed.Equal(*b.Feed.Items[0].PublishedParsed) {
		t.Fatal("publication date changed")
	}
	if !strings.Contains(string(second), "updated") || !strings.Contains(string(second), feed.AtomNS) {
		t.Fatal("content change did not receive a separate modification timestamp")
	}
	repeated := commit(t, db, id, document(t, "New", "new"))
	if string(second) != string(repeated) {
		t.Fatal("unchanged article received a new modification timestamp")
	}
	// A rule change on a known article preserves its existing proxy identity.
	c, _ = db.ByID(ctx, id)
	c.Identity.Kind, c.Identity.Regex = "full", ""
	if _, err := db.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	third, _ := feed.Parse(commit(t, db, id, document(t, "New", "new")))
	if third.Feed.Items[0].GUID != a.Feed.Items[0].GUID {
		t.Fatal("rule edit changed a known article's GUID")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrations are not repeatable: %v", err)
	}
}

func TestDuplicateSourceEntriesHaveStableModificationTime(t *testing.T) {
	db := testutil.Database(t)
	id, err := db.Save(context.Background(), configuration())
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, "First occurrence", "first")
	last := *doc.Feed.Items[0]
	last.Title, last.GUID = "Last occurrence", "https://example.org/last-123.html"
	doc.Feed.Items = append(doc.Feed.Items, &last)
	doc.ItemExtras = append(doc.ItemExtras, nil)
	first := commit(t, db, id, doc)
	second := commit(t, db, id, doc)
	if string(first) != string(second) {
		t.Fatal("duplicate source entries made an unchanged feed appear modified")
	}
}

func TestModificationTimestampSurvivesPostgresPrecision(t *testing.T) {
	db := testutil.Database(t)
	id, err := db.Save(context.Background(), configuration())
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, "Title", "heading")
	nanoseconds := time.Date(2026, 11, 16, 12, 0, 0, 123456789, time.UTC)
	doc.Feed.Items[0].UpdatedParsed = &nanoseconds
	first := commit(t, db, id, doc)
	second := commit(t, db, id, doc)
	if string(first) != string(second) {
		t.Fatal("PostgreSQL timestamp precision changed the output on the next request")
	}
}

func TestFailedCommitRollsBackAndSourceChangeResetsValidators(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	id, err := db.Save(ctx, configuration())
	if err != nil {
		t.Fatal(err)
	}
	commit(t, db, id, document(t, "Original", "old"))
	l, err := db.Lock(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	invalid := document(t, "New", "new")
	invalid.Feed.Items[0].GUID = "unmatched"
	if _, err := l.Commit(ctx, invalid, fetch.Validators{ETag: `"bad"`}); err == nil {
		t.Fatal("invalid identity accepted")
	}
	state, err := l.State(ctx)
	l.Close()
	if err != nil || state.Document.Feed.Items[0].Title != "Original" || state.Validators.ETag != `"source"` {
		t.Fatalf("failed commit changed state: %v", err)
	}
	c, _ := db.ByID(ctx, id)
	stale := c
	c.URL = "https://example.org/another-source"
	if _, err := db.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Save(ctx, stale); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stale configuration overwrote a newer edit")
	}
	l, _ = db.Lock(ctx, id)
	state, err = l.State(ctx)
	l.Close()
	if err != nil || state.Document != nil || state.Validators.ETag != "" {
		t.Fatal("validators survived a source URL change")
	}
}

func TestLockCancellationDoesNotLeakSessionLock(t *testing.T) {
	db := testutil.Database(t)
	id, err := db.Save(context.Background(), configuration())
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.Lock(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if l, err := db.Lock(ctx, id); err == nil {
		l.Close()
		t.Fatal("concurrent lock unexpectedly acquired")
	}
	first.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	next, err := db.Lock(ctx2, id)
	if err != nil {
		t.Fatalf("cancelled lock leaked: %v", err)
	}
	next.Close()
}

func TestLegacyImportIsReadOnlyAndIdempotent(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `CREATE TABLE rssDedupConfig(
		id BIGINT PRIMARY KEY,httpEndpoint TEXT,feedName TEXT,feedURL TEXT,
		feedIDFromStartOrEnd TEXT,feedIDLength INT,feedIDFromStartOrEndLength INT,
		feedIconURL TEXT,artikelImageTag TEXT);
		INSERT INTO rssDedupConfig VALUES
		(7,'/old-feed','Legacy','https://example.org/rss',
		'End',3,8,'https://example.org/custom.png',' media:content '),
		(8,'/no-image','HEISE','https://example.org/heise',
		'End',3,8,'Existing','NoImage')`)
	if err != nil {
		t.Fatal(err)
	}
	count, err := db.ImportLegacy(ctx, "")
	if err != nil || count != 2 {
		t.Fatalf("import: count=%d err=%v", count, err)
	}
	c, err := db.ByPath(ctx, "/old-feed")
	if err != nil || c.Identity.Kind != "legacy" || c.Overrides.ImageURL.Value != "https://example.org/custom.png" {
		t.Fatalf("bad import: %+v %v", c, err)
	}
	if c.Overrides.ItemImageSource != "media:content" {
		t.Fatalf("image source was not trimmed: %q", c.Overrides.ItemImageSource)
	}
	noImage, err := db.ByPath(ctx, "/no-image")
	if err != nil || noImage.Overrides.ItemImage.Mode != "remove" || noImage.Overrides.ItemImageSource != "auto" {
		t.Fatalf("NoImage was not converted to image removal: %+v %v", noImage, err)
	}
	if err := noImage.Validate(); err != nil {
		t.Fatalf("NoImage produced an invalid configuration: %v", err)
	}
	key, err := c.Identity.Key("prefix-123-tail", "")
	if err != nil || key != "123" {
		t.Fatal("legacy extraction changed")
	}
	c.Name = "Edited after import"
	if _, err := db.Save(ctx, c); err != nil {
		t.Fatal(err)
	}
	if count, err := db.ImportLegacy(ctx, ""); err != nil || count != 0 {
		t.Fatalf("repeated import: count=%d err=%v", count, err)
	}
	c, _ = db.ByID(ctx, c.ID)
	if c.Name != "Edited after import" {
		t.Fatal("import overwrote user edits")
	}
	var name string
	if err := db.Pool.QueryRow(ctx, `SELECT feedName FROM rssDedupConfig WHERE id=7`).Scan(&name); err != nil || name != "Legacy" {
		t.Fatal("legacy table changed")
	}
	var imageTag string
	if err := db.Pool.QueryRow(ctx, `SELECT artikelImageTag FROM rssDedupConfig WHERE id=8`).Scan(&imageTag); err != nil || imageTag != "NoImage" {
		t.Fatal("legacy NoImage setting changed")
	}
}

func TestInvalidImportIsAtomic(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `CREATE TABLE rssDedupConfig(
		id BIGINT PRIMARY KEY,httpEndpoint TEXT,feedName TEXT,feedURL TEXT,
		feedIDFromStartOrEnd TEXT,feedIDLength INT,feedIDFromStartOrEndLength INT,
		feedIconURL TEXT,artikelImageTag TEXT);
		INSERT INTO rssDedupConfig VALUES
		(1,'/valid','Valid','https://example.org/rss','End',3,8,'Existing','enclosure'),
		(2,'/invalid','Invalid','https://example.org/rss','End',0,0,'Existing','enclosure')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportLegacy(ctx, ""); err == nil || !strings.Contains(err.Error(), "2") {
		t.Fatal("invalid import should fail visibly")
	}
	list, err := db.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal("partial legacy import persisted")
	}
}
