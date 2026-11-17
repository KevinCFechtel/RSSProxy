package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/app"
	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
	"github.com/KevinCFechtel/RSSProxy/internal/testutil"
)

func sourceDocument(title, slug, publication string, present bool) string {
	item := ""
	if present {
		item = fmt.Sprintf(`<item><title>%s</title><guid isPermaLink="true">https://example.org/%s-123.html</guid>
			<link>https://example.org/%s-123.html</link><description>Text %s</description><pubDate>%s</pubDate></item>`, title, slug, slug, title, publication)
	}
	return `<rss version="2.0"><channel><title>Source</title><link>https://example.org</link><description>News</description>` + item + `</channel></rss>`
}

func handler(t *testing.T, db *store.Store) http.Handler {
	t.Helper()
	h, err := app.Handler(app.Config{
		AuthUser: "admin", AuthPassword: "test-password",
		SourceTimeout: time.Second, RequestTimeout: 15 * time.Second, MaxFeedBytes: 1 << 20,
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, path, etag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func parseResponse(t *testing.T, w *httptest.ResponseRecorder) *feed.Document {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	doc, err := feed.Parse(w.Body.Bytes())
	if err != nil {
		t.Fatalf("parse generated feed: %v\n%s", err, w.Body.String())
	}
	return doc
}

func TestReaderLifecycleAndSourceErrors(t *testing.T) {
	db := testutil.Database(t)
	var mu sync.Mutex
	calls, mode := 0, "original"
	pub := "Mon, 16 Nov 2026 12:00:00 +0000"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch mode {
		case "error":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "limited":
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		case "304":
			if r.Header.Get("If-None-Match") == "" {
				t.Error("missing upstream validator")
			}
			w.WriteHeader(http.StatusNotModified)
		case "invalid":
			_, _ = w.Write([]byte("<not-a-feed>"))
		case "missing":
			w.Header().Set("ETag", `"empty"`)
			_, _ = w.Write([]byte(sourceDocument("", "", pub, false)))
		case "updated":
			w.Header().Set("ETag", `"updated"`)
			_, _ = w.Write([]byte(sourceDocument("Updated", "new-heading", "Tue, 17 Nov 2026 12:00:00 +0000", true)))
		default:
			w.Header().Set("ETag", `"original"`)
			_, _ = w.Write([]byte(sourceDocument("Original", "old-heading", pub, true)))
		}
	}))
	defer upstream.Close()
	c := feed.DefaultConfig()
	c.Name, c.Path, c.URL = "Example", "/news", upstream.URL
	c.Identity = feed.IdentityRule{Source: "guid", Kind: "regex", Regex: `-(?P<id>[0-9]+)\.html$`, Hosts: []string{"example.org"}}
	id, err := db.Save(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	h := handler(t, db)
	firstResponse := get(h, "/news", "")
	first := parseResponse(t, firstResponse)
	guid := first.Feed.Items[0].GUID
	second := parseResponse(t, get(h, "/news", ""))
	if second.Feed.Items[0].GUID != guid || calls != 2 {
		t.Fatal("repeat reader call consumed article or skipped source")
	}
	setMode := func(value string) {
		mu.Lock()
		mode = value
		mu.Unlock()
	}
	setMode("updated")
	updated := parseResponse(t, get(h, "/news", firstResponse.Header().Get("ETag")))
	if updated.Feed.Items[0].GUID != guid || updated.Feed.Items[0].Title != "Updated" ||
		!updated.Feed.Items[0].PublishedParsed.Equal(*first.Feed.Items[0].PublishedParsed) {
		t.Fatal("article update did not retain identity and original publication date")
	}
	setMode("missing")
	if len(parseResponse(t, get(h, "/news", "")).Feed.Items) != 0 {
		t.Fatal("article no longer in source remained in output")
	}
	setMode("updated")
	if parseResponse(t, get(h, "/news", "")).Feed.Items[0].GUID != guid {
		t.Fatal("reappearing article lost proxy identity")
	}
	setMode("304")
	stored := get(h, "/news", "")
	parseResponse(t, stored)
	conditional := get(h, "/news", stored.Header().Get("ETag"))
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("reader conditional response: %d %s", conditional.Code, conditional.Body.String())
	}
	// Even matching reader validators must not hide a source error.
	for _, tt := range []struct {
		mode string
		want int
	}{
		{"error", 503}, {"limited", 429}, {"invalid", 502},
	} {
		setMode(tt.mode)
		w := get(h, "/news", stored.Header().Get("ETag"))
		if w.Code != tt.want || strings.Contains(w.Body.String(), "<rss") {
			t.Errorf("source %s yielded HTTP %d: %s", tt.mode, w.Code, w.Body.String())
		}
		if tt.mode == "limited" && w.Header().Get("Retry-After") != "60" {
			t.Error("Retry-After missing")
		}
	}
	// A config-only change applies to a stored document following upstream 304.
	c, _ = db.ByID(context.Background(), id)
	c.Overrides.Title = feed.Override{Mode: "replace", Value: "My title"}
	if _, err := db.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	setMode("304")
	if doc := parseResponse(t, get(h, "/news", stored.Header().Get("ETag"))); doc.Feed.Title != "My title" {
		t.Fatal("replacement did not apply to stored source document")
	}
	list, _ := db.List(context.Background())
	if list[0].State.LastError != "" {
		t.Fatal("successful retry did not clear error status")
	}
	mu.Lock()
	totalCalls := calls
	mu.Unlock()
	if totalCalls != 11 {
		t.Fatalf("expected one source call per reader request, got %d", totalCalls)
	}
}

func TestConcurrentRequestsEachFetchButShareIdentity(t *testing.T) {
	db := testutil.Database(t)
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = w.Write([]byte(sourceDocument("Title", "heading", "Mon, 16 Nov 2026 12:00:00 +0000", true)))
	}))
	defer upstream.Close()
	c := feed.DefaultConfig()
	c.Name, c.Path, c.URL = "Example", "/news", upstream.URL
	if _, err := db.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h := handler(t, db)
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for range 8 {
		wg.Go(func() { responses <- get(h, "/news", "") })
	}
	wg.Wait()
	close(responses)
	guid := ""
	for response := range responses {
		doc := parseResponse(t, response)
		if guid != "" && guid != doc.Feed.Items[0].GUID {
			t.Fatal("concurrent requests assigned different GUIDs")
		}
		guid = doc.Feed.Items[0].GUID
	}
	if calls != 8 {
		t.Fatalf("source requests were coalesced: %d", calls)
	}
	var count int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM rssproxy_identities`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate identity records: %d %v", count, err)
	}
}

func adminRequest(h http.Handler, method, path string, values url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	req.SetBasicAuth("admin", "test-password")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminAuthCSRFPreviewAndCRUD(t *testing.T) {
	db := testutil.Database(t)
	h := handler(t, db)
	if w := get(h, "/admin/", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("administration accessible without authentication")
	}
	w := adminRequest(h, http.MethodGet, "/admin/feeds/new", nil, nil, "")
	if w.Code != 200 {
		t.Fatalf("form failed: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing CSRF cookie")
	}
	token := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if len(token) != 2 {
		t.Fatal("missing CSRF form token")
	}
	values := url.Values{
		"csrf": {token[1]}, "name": {"<script>alert(1)</script>"}, "path": {"/test-feed"},
		"url": {"https://example.org/rss"}, "identity_source": {"guid"},
		"identity_kind": {"regex"}, "identity_regex": {`-(?P<id>[0-9]+)\.html$`},
		"item_image_source": {"auto"}, "preview_guids": {"https://example.org/title-123.html"},
	}
	if w := adminRequest(h, http.MethodPost, "/admin/feeds/save", values, nil, ""); w.Code != 403 {
		t.Fatal("missing CSRF cookie accepted")
	}
	if w := adminRequest(h, http.MethodPost, "/admin/feeds/save", values, cookies[0], "https://evil.example"); w.Code != 403 {
		t.Fatal("cross-site form accepted")
	}
	w = adminRequest(h, http.MethodPost, "/admin/feeds/preview", values, cookies[0], "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<code>123</code>") {
		t.Fatalf("preview failed: %d %s", w.Code, w.Body.String())
	}
	if configs, _ := db.List(context.Background()); len(configs) != 0 {
		t.Fatal("preview saved a configuration")
	}
	w = adminRequest(h, http.MethodPost, "/admin/feeds/save", values, cookies[0], "")
	if w.Code != 303 {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	w = adminRequest(h, http.MethodGet, "/admin/", nil, cookies[0], "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>alert") {
		t.Fatalf("administration did not escape feed name: %s", w.Body.String())
	}
	c, err := db.ByPath(context.Background(), "/test-feed")
	if err != nil {
		t.Fatal(err)
	}
	w = adminRequest(h, http.MethodGet, fmt.Sprintf("/admin/feeds/%d", c.ID), nil, cookies[0], "")
	if w.Code != 200 {
		t.Fatal("edit form failed")
	}
	deleteValues := url.Values{"csrf": {token[1]}, "revision": {fmt.Sprint(c.Revision)}}
	w = adminRequest(h, http.MethodPost, fmt.Sprintf("/admin/feeds/%d/delete", c.ID), deleteValues, cookies[0], "")
	if w.Code != 303 {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body.String())
	}
	if w := get(h, "/test-feed", ""); w.Code != 404 {
		t.Fatal("deleted feed still exists")
	}
	if w := get(h, "/readyz", ""); w.Code != 200 {
		t.Fatal("readiness failed")
	}
}
