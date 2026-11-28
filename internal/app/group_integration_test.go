package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
	"github.com/KevinCFechtel/RSSProxy/internal/testutil"
)

func TestMergedGroupsEndToEnd(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	var mu sync.Mutex
	count := map[string]int{}
	failPolitics := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count[r.URL.Path]++
		fail := failPolitics && r.URL.Path == "/politics"
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("If-None-Match") == `"source-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"source-1"`)
		if r.URL.Path == "/news" {
			_, _ = w.Write([]byte(`<rss version="2.0"><channel><title>News</title><link>https://example.org</link><description>News</description>
<item><title>Shared</title><guid>shared-id</guid><link>https://example.org/shared</link><category>Ausland</category></item>
<item><title>News only</title><guid>news-id</guid><link>https://example.org/news</link></item></channel></rss>`))
		} else {
			_, _ = w.Write([]byte(`<rss version="2.0"><channel><title>Politics</title><link>https://example.org</link><description>Politics</description>
<item><title>Shared in politics</title><guid>shared-id</guid><link>https://example.org/shared</link><category>Politik</category></item>
<item><title>Politics only</title><guid>politics-id</guid><link>https://example.org/politics</link></item></channel></rss>`))
		}
	}))
	defer upstream.Close()
	news := feed.DefaultConfig()
	news.Name, news.Path, news.URL = "News", "/news", upstream.URL+"/news"
	newsID, err := db.Save(ctx, news)
	if err != nil {
		t.Fatal(err)
	}
	politics := feed.DefaultConfig()
	politics.Name, politics.Path, politics.URL = "Politics", "/politics", upstream.URL+"/politics"
	politicsID, err := db.Save(ctx, politics)
	if err != nil {
		t.Fatal(err)
	}
	group := feed.DefaultConfig()
	group.Type, group.Name, group.Path = "group", "Combined", "/merged"
	group.SourceIDs = []int64{newsID, politicsID}
	groupID, err := db.Save(ctx, group)
	if err != nil {
		t.Fatal(err)
	}
	h := handler(t, db)
	firstResponse := get(h, "/merged", "")
	first := parseResponse(t, firstResponse)
	if len(first.Feed.Items) != 3 {
		t.Fatalf("want 3 unique items, got %d", len(first.Feed.Items))
	}
	var sharedGUID string
	for _, item := range first.Feed.Items {
		if item.Title == "Shared" {
			sharedGUID = item.GUID
			cats := strings.Join(item.Categories, ",")
			if !strings.Contains(cats, "Ausland") || !strings.Contains(cats, "Politik") {
				t.Fatalf("missing categories: %s", cats)
			}
		}
	}
	if sharedGUID == "" {
		t.Fatal("shared article not present")
	}
	second := get(h, "/merged", firstResponse.Header().Get("ETag"))
	if second.Code != http.StatusNotModified {
		t.Fatalf("reader ETag status=%d, want 304", second.Code)
	}
	mu.Lock()
	n, p := count["/news"], count["/politics"]
	mu.Unlock()
	if n != 2 || p != 2 {
		t.Fatalf("did not re-fetch both sources: news=%d politics=%d", n, p)
	}
	third := parseResponse(t, get(h, "/merged", ""))
	for _, item := range third.Feed.Items {
		if item.Title == "Shared" && item.GUID != sharedGUID {
			t.Fatal("group GUID not stable")
		}
	}
	mu.Lock()
	failPolitics = true
	mu.Unlock()
	if w := get(h, "/merged", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial group was returned as success: %d", w.Code)
	}
	mu.Lock()
	failPolitics = false
	mu.Unlock()
	if len(parseResponse(t, get(h, "/merged", "")).Feed.Items) != 3 {
		t.Fatal("failed request corrupted group snapshot")
	}
	if len(parseResponse(t, get(h, "/news", "")).Feed.Items) != 2 {
		t.Fatal("original feed endpoint changed")
	}
	current, err := db.ByID(ctx, newsID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(ctx, newsID, current.Revision); err != store.ErrConflict {
		t.Fatalf("referenced source deletion should be blocked: %v", err)
	}
	// The group is independently configurable and can be deleted without
	// deleting any source. Its public endpoint disappears afterward.
	currentGroup, _ := db.ByID(ctx, groupID)
	if err := db.Delete(ctx, groupID, currentGroup.Revision); err != nil {
		t.Fatal(err)
	}
	if w := get(h, "/merged", ""); w.Code != http.StatusNotFound {
		t.Fatal("deleted group still served")
	}
	if w := get(h, "/news", ""); w.Code != http.StatusOK {
		t.Fatal("deleting a group deleted its source")
	}
}

func TestAdminCanConfigureGroup(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	source := feed.DefaultConfig()
	source.Name, source.Path, source.URL = "Sample", "/sample", "https://example.org/source"
	sourceID, err := db.Save(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	h := handler(t, db)
	w := adminRequest(h, http.MethodGet, "/admin/groups/new", nil, nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="source_ids"`) {
		t.Fatalf("group creation page unavailable: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	group := url.Values{
		"csrf": {cookie.Value}, "type": {"group"}, "name": {"Sample group"},
		"path": {"/sample-group"}, "source_ids": {""},
		"identity_source": {"guid"}, "identity_kind": {"full"},
		"item_image_source": {"auto"},
	}
	group.Set("source_ids", strconv.FormatInt(sourceID, 10))
	saved := adminRequest(h, http.MethodPost, "/admin/feeds/save", group, cookie, "")
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("group save failed: %d %s", saved.Code, saved.Body.String())
	}
	result, err := db.ByPath(ctx, "/sample-group")
	if err != nil || result.Type != "group" || len(result.SourceIDs) != 1 || result.SourceIDs[0] != sourceID {
		t.Fatalf("saved group invalid: %#v %v", result, err)
	}
}
