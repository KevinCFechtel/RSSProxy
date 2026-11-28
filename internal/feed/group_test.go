package feed

import (
	"strings"
	"testing"
)

func TestGroupMergeUniqueArticlesCategoriesAndRelativeURLs(t *testing.T) {
	t.Parallel()
	a, err := Parse([]byte(`<rss version="2.0"><channel><title>A</title><link>https://news.example.org</link><description>News</description>
<item><title>First version</title><guid>common</guid><link>/article/common</link><category domain="section">Ausland</category><pubDate>Mon, 16 Nov 2026 12:00:00 +0000</pubDate></item>
<item><title>A only</title><guid>a-only</guid><link>/article/a</link><category>Nachrichten</category><pubDate>Mon, 16 Nov 2026 13:00:00 +0000</pubDate></item>
</channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`<rss version="2.0"><channel><title>B</title><link>https://politics.example.org</link><description>Politics</description>
<item><title>Other version</title><guid>common</guid><link>/other/article</link><category>Politik</category><pubDate>Mon, 16 Nov 2026 12:00:00 +0000</pubDate></item>
<item><title>B only</title><guid>b-only</guid><link>article/b</link><category>Inland</category><enclosure url="audio/episode.mp3" type="audio/mpeg" length="123"/><pubDate>Mon, 16 Nov 2026 14:00:00 +0000</pubDate></item>
</channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.Type, c.Name, c.Path, c.SourceIDs = "group", "Merged", "/merged", []int64{1, 2}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	merged, err := MergeGroup(c, []GroupSource{
		{URL: "https://news.example.org/rss", Document: a},
		{URL: "https://politics.example.org/feeds/", Document: b},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(merged.Feed.Items); got != 3 {
		t.Fatalf("articles=%d, want 3", got)
	}
	if merged.Feed.Items[0].GUID != "b-only" || merged.Feed.Items[1].GUID != "a-only" || merged.Feed.Items[2].GUID != "common" {
		t.Fatalf("chronological sort failed: %#v", merged.Feed.Items)
	}
	if merged.Feed.Items[2].Title != "First version" {
		t.Fatal("source priority was lost")
	}
	if !strings.Contains(strings.Join(merged.Feed.Items[2].Categories, ","), "Politik") {
		t.Fatal("categories were not merged")
	}
	ids := map[string]Identity{
		"common": {Key: "common", GUID: "urn:uuid:common"},
		"a-only": {Key: "a-only", GUID: "urn:uuid:a"},
		"b-only": {Key: "b-only", GUID: "urn:uuid:b"},
	}
	output, err := Render(c, merged, ids)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `category domain="section"`) || !strings.Contains(string(output), ">Politik</category>") {
		t.Fatalf("original categories were not retained: %s", output)
	}
	if !strings.Contains(string(output), "https://politics.example.org/feeds/audio/episode.mp3") {
		t.Fatalf("second source's enclosure URL was not resolved: %s", output)
	}
	parsed, err := Parse(output)
	if err != nil || len(parsed.Feed.Items) != 3 {
		t.Fatalf("output invalid: %v", err)
	}
	if parsed.Feed.Items[2].GUID != "urn:uuid:common" {
		t.Fatal("persisted identity missing")
	}
	if a.Feed.Items[0].Title != "First version" || a.Feed.Items[0].Link != "/article/common" {
		t.Fatal("source document was mutated")
	}
}

func TestGroupRejectsBadMembersAndIdentity(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Type, c.Name, c.Path, c.SourceIDs = "group", "Merged", "/merged", []int64{1, 1}
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate source IDs accepted")
	}
	c.SourceIDs = []int64{1}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	src, err := Parse([]byte(`<rss version="2.0"><channel><title>A</title><link>https://a.example</link><description>X</description><item><title>X</title><link>https://a.example/article</link></item></channel></rss>`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeGroup(c, []GroupSource{{URL: "https://a.example/feed", Document: src}}); err == nil {
		t.Fatal("missing source GUID should fail visibly")
	}
}
