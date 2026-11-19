package feed

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

const podcastFixture = `<?xml version="1.0"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"
 xmlns:media="http://search.yahoo.com/mrss/"
 xmlns:itunes="http://www.itunes.com/DTDs/PodCast-1.0.dtd"
 xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Source</title><link>https://example.org</link><description>News</description>
<image><url>https://example.org/old.png</url><title>Original artwork</title><link>https://example.org/about</link></image>
<itunes:image href="https://example.org/old.png"/><itunes:category text="News"><itunes:category text="Daily News"/></itunes:category>
<podcast:locked owner="editor@example.org">yes</podcast:locked>
<item><title>Original title</title><link>https://example.org/old-123.html</link>
<guid isPermaLink="true">https://example.org/old-123.html</guid>
<description>Original description</description><content:encoded><![CDATA[<p>Full text &amp; more</p>]]></content:encoded>
<pubDate>Mon, 16 Nov 2026 12:00:00 +0000</pubDate>
<category domain="topic">Science</category><author>editor@example.org (Editor)</author>
<media:thumbnail url="https://example.org/old.jpg" width="300"/>
<itunes:duration>01:23:45</itunes:duration><itunes:episode>42</itunes:episode>
<podcast:transcript url="https://example.org/transcript.vtt" type="text/vtt"/>
<enclosure url="https://example.org/episode.mp3" type="audio/mpeg" length="123456"/>
</item></channel></rss>`

func fixture(t *testing.T, source string) (Config, *Document, map[string]Identity) {
	t.Helper()
	c := DefaultConfig()
	c.Name, c.Path, c.URL = "Example", "/feeds/example", "https://example.org/rss"
	c.Identity = IdentityRule{Source: "guid", Kind: "regex", Regex: `-(?P<id>[0-9]+)\.html$`}
	doc, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	published := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	return c, doc, map[string]Identity{"123": {Key: "123", GUID: "urn:uuid:test-123", Published: &published}}
}

func TestRenderPreservesPodcastAndOriginals(t *testing.T) {
	t.Parallel()
	c, doc, ids := fixture(t, podcastFixture)
	before, err := xml.Marshal(doc.Extras)
	if err != nil {
		t.Fatal(err)
	}
	c.Overrides.Title = Override{Mode: "replace", Value: "My feed"}
	c.Overrides.ImageURL = Override{Mode: "replace", Value: "https://cdn.example.org/logo.png"}
	c.Overrides.ItemImage = Override{Mode: "replace", Value: "https://cdn.example.org/article.png"}
	body, err := Render(c, doc, ids)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Parse(body)
	if err != nil {
		t.Fatalf("generated RSS cannot be parsed: %v\n%s", err, body)
	}
	if output.Feed.Title != "My feed" || output.Feed.Image.URL != c.Overrides.ImageURL.Value {
		t.Fatalf("feed override failed: %#v", output.Feed)
	}
	item := output.Feed.Items[0]
	if item.ITunesExt == nil || item.ITunesExt.Duration != "01:23:45" {
		t.Fatalf("iTunes metadata not recognized by parser after round trip: %s", body)
	}
	if output.Feed.ITunesExt == nil || output.Feed.ITunesExt.Image != c.Overrides.ImageURL.Value {
		t.Fatalf("podcast artwork override not recognized by parser: %s", body)
	}
	if item.GUID != "urn:uuid:test-123" || !strings.Contains(string(body), `isPermaLink="false"`) {
		t.Fatalf("proxy GUID missing: %s", body)
	}
	if len(item.Enclosures) != 1 || item.Enclosures[0].URL != "https://example.org/episode.mp3" || item.Enclosures[0].Length != "123456" {
		t.Fatalf("podcast enclosure changed: %#v", item.Enclosures)
	}
	for _, value := range []string{"01:23:45", "transcript.vtt", "Daily News", "podcastindex.org", "topic", "<p>Full text &amp; more</p>"} {
		if !strings.Contains(string(body), value) && value != "<p>Full text &amp; more</p>" {
			t.Errorf("metadata %q missing from %s", value, body)
		}
	}
	if item.Content != doc.Feed.Items[0].Content {
		t.Errorf("HTML content changed: %q -> %q", doc.Feed.Items[0].Content, item.Content)
	}
	if strings.Contains(string(body), "old.jpg") || strings.Contains(string(body), "old.png") {
		t.Errorf("old artwork still present: %s", body)
	}
	after, _ := xml.Marshal(doc.Extras)
	if !bytes.Equal(before, after) || doc.Feed.Title != "Source" {
		t.Fatal("rendering mutated source data")
	}
	body2, err := Render(c, doc, ids)
	if err != nil || !bytes.Equal(body, body2) {
		t.Fatal("rendering is not deterministic")
	}
}

func TestLogoReplacementWithEmptyChannelDescription(t *testing.T) {
	t.Parallel()
	for _, description := range []string{"<description />", "<description></description>", ""} {
		t.Run(description, func(t *testing.T) {
			// Capital's RSS feed has a title and website link, but an empty description.
			source := `<rss version="2.0"><channel><title>Capital.de</title>
<link>https://www.capital.de/</link>` + description + `
<image><url>https://example.org/original.png</url><title>Capital.de</title><link>https://www.capital.de/</link></image>
<item><title>Article</title><guid>https://www.capital.de/article-123.html</guid>
<description>Article text</description>
<enclosure url="https://example.org/article.jpg" type="image/jpeg" length="0"/>
</item></channel></rss>`
			c, doc, ids := fixture(t, source)
			c.Name, c.Path, c.URL = "Capital", "/capital", "https://www.capital.de/feed/standard/"
			c.Overrides.ImageURL = Override{Mode: "replace", Value: "https://example.org/replacement.png"}
			body, err := Render(c, doc, ids)
			if err != nil {
				t.Fatal(err)
			}
			output, err := Parse(body)
			if err != nil {
				t.Fatalf("generated RSS cannot be parsed: %v\n%s", err, body)
			}
			if output.Feed.Title != doc.Feed.Title || output.Feed.Link != doc.Feed.Link ||
				output.Feed.Description != "" || !bytes.Contains(body, []byte("<description></description>")) {
				t.Fatalf("channel metadata changed or description element omitted: %s", body)
			}
			if output.Feed.Image == nil || output.Feed.Image.URL != c.Overrides.ImageURL.Value {
				t.Fatalf("logo was not replaced: %s", body)
			}
			item := output.Feed.Items[0]
			if item.Title != doc.Feed.Items[0].Title || item.Description != "Article text" ||
				len(item.Enclosures) != 1 || item.Enclosures[0].URL != "https://example.org/article.jpg" {
				t.Fatalf("logo replacement changed article metadata: %s", body)
			}
			if doc.Feed.Description != "" || doc.Feed.Image.URL != "https://example.org/original.png" {
				t.Fatal("rendering mutated source metadata")
			}

			c.Overrides.Description = Override{Mode: "fallback", Value: "Configured description"}
			body, err = Render(c, doc, ids)
			if err != nil {
				t.Fatal(err)
			}
			output, err = Parse(body)
			if err != nil || output.Feed.Description != "Configured description" {
				t.Fatalf("explicit description fallback failed: %v\n%s", err, body)
			}
		})
	}
}

func TestRenderStillRequiresChannelTitleAndWebsiteLink(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"title", "link"} {
		t.Run(field, func(t *testing.T) {
			c, doc, ids := fixture(t, podcastFixture)
			if field == "title" {
				doc.Feed.Title = ""
			} else {
				doc.Feed.Link = ""
			}
			if _, err := Render(c, doc, ids); err == nil {
				t.Fatalf("missing channel %s accepted", field)
			}
		})
	}
}

func TestOptionalFieldsAndDuplicateItems(t *testing.T) {
	t.Parallel()
	source := `<rss version="2.0"><channel><title>Feed</title><link>https://example.org</link><description>News</description>
<item><title>Old</title><guid>https://example.org/old-123.html</guid></item>
<item><title>New</title><guid>https://example.org/new-123.html</guid></item></channel></rss>`
	c, doc, ids := fixture(t, source)
	ids["123"] = Identity{Key: "123", GUID: "urn:uuid:123"}
	body, err := Render(c, doc, ids)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Feed.Items) != 1 || parsed.Feed.Items[0].Title != "New" || parsed.Feed.Items[0].PublishedParsed != nil {
		t.Fatalf("optional fields or deduplication failed: %s", body)
	}
	if parsed.Feed.Image != nil {
		t.Fatal("missing source image should remain absent")
	}
}

func TestOverrideModes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ mode, original, value, want string }{
		{"original", "old", "new", "old"}, {"replace", "old", "new", "new"},
		{"fallback", "old", "new", "old"}, {"fallback", "", "new", "new"},
		{"remove", "old", "", ""},
	} {
		if got := (Override{Mode: tt.mode, Value: tt.value}).Apply(tt.original); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.mode, got, tt.want)
		}
	}
	c, doc, ids := fixture(t, podcastFixture)
	c.Overrides.ImageURL = Override{Mode: "remove"}
	c.Overrides.ItemImage = Override{Mode: "remove"}
	body, err := Render(c, doc, ids)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "old.jpg") || strings.Contains(string(body), "old.png") ||
		!strings.Contains(string(body), "episode.mp3") {
		t.Fatalf("removing images damaged media: %s", body)
	}
}

func TestAtomArtworkAndUpdates(t *testing.T) {
	t.Parallel()
	source := `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><subtitle>News</subtitle>
<link href="https://example.org"/><id>https://example.org/feed</id><updated>2026-11-16T13:00:00Z</updated>
<icon>https://example.org/icon.png</icon><logo>https://example.org/logo.png</logo>
<entry><id>https://example.org/story-123.html</id><title>Updated</title><link href="https://example.org/story-123.html"/>
<updated>2026-11-16T13:00:00Z</updated><content type="html">&lt;p&gt;Updated text&lt;/p&gt;</content></entry></feed>`
	c, doc, ids := fixture(t, source)
	body, err := Render(c, doc, ids)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "logo.png") || !strings.Contains(string(body), "2026-11-16T13:00:00Z") {
		t.Fatalf("Atom metadata missing: %s", body)
	}
}

func TestRSS1AndJSONSources(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">
<channel rdf:about="https://example.org/rss"><title>RSS1</title><link>https://example.org</link><description>News</description>
<items><rdf:Seq><rdf:li rdf:resource="https://example.org/story-123.html"/></rdf:Seq></items></channel>
<item rdf:about="https://example.org/story-123.html"><title>Article</title><link>https://example.org/story-123.html</link><description>Text</description></item>
</rdf:RDF>`,
		`{"version":"https://jsonfeed.org/version/1.1","title":"JSON","description":"News","home_page_url":"https://example.org",
		"items":[{"id":"https://example.org/story-123.html","url":"https://example.org/story-123.html","title":"Article","content_text":"Text"}]}`,
	} {
		c, doc, ids := fixture(t, source)
		c.Identity.Source = "link" // RSS 1.0 need not contain an RSS 2.0 GUID.
		body, err := Render(c, doc, ids)
		if err != nil {
			t.Fatal(err)
		}
		output, err := Parse(body)
		if err != nil || len(output.Feed.Items) != 1 || output.Feed.Items[0].GUID != ids["123"].GUID {
			t.Fatalf("source conversion failed: %v\n%s", err, body)
		}
	}
}

func TestImageEnclosuresDoNotOverrideReplacementOrFallback(t *testing.T) {
	t.Parallel()
	source := strings.Replace(podcastFixture, `<enclosure url="https://example.org/episode.mp3"`,
		`<enclosure url="https://example.org/picture.jpg" type="image/jpeg" length="123"/><enclosure url="https://example.org/episode.mp3"`, 1)
	c, doc, ids := fixture(t, source)
	c.Overrides.ItemImage = Override{Mode: "remove"}
	body, err := Render(c, doc, ids)
	if err != nil || strings.Contains(string(body), "picture.jpg") || !strings.Contains(string(body), "episode.mp3") {
		t.Fatalf("image enclosure removal failed: %v %s", err, body)
	}
}

func TestUnknownExtensionNamespacesAndUnprefixedChildren(t *testing.T) {
	t.Parallel()
	source := strings.Replace(podcastFixture, "</item>",
		`<custom:extension xmlns:custom="urn:example:custom" xmlns:meta="urn:example:attributes" meta:version="1">
<label>Unprefixed child</label><custom:child>Namespaced child</custom:child></custom:extension></item>`, 1)
	c, doc, ids := fixture(t, source)
	body, err := Render(c, doc, ids)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range output.ItemExtras[0] {
		if e.Name.Space != "urn:example:custom" || e.Name.Local != "extension" {
			continue
		}
		foundLabel, foundChild, foundAttribute := false, false, false
		for _, attr := range e.Attrs {
			foundAttribute = foundAttribute || (attr.Name.Space == "urn:example:attributes" && attr.Name.Local == "version" && attr.Value == "1")
		}
		for _, part := range e.Parts {
			if part.Child != nil {
				foundLabel = foundLabel || (part.Child.Name.Local == "label" && part.Child.Name.Space == "")
				foundChild = foundChild || (part.Child.Name.Local == "child" && part.Child.Name.Space == "urn:example:custom")
			}
		}
		if !foundLabel || !foundChild || !foundAttribute {
			t.Fatalf("namespace semantics changed: %+v\n%s", e, body)
		}
		return
	}
	t.Fatal("custom extension disappeared")
}

func TestParseRejectsMalformedXML(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		`<!DOCTYPE rss [<!ENTITY test "bad">]><rss><channel><title>&test;</title></channel></rss>`,
		`<rss><channel></rss>`,
		`<rss><channel/></rss><second/>`,
		strings.Repeat("<rss>", 100) + strings.Repeat("</rss>", 100),
	} {
		if _, err := Parse([]byte(source)); err == nil {
			t.Errorf("invalid XML accepted")
		}
	}
}

func TestUTF8ByteOrderMark(t *testing.T) {
	t.Parallel()
	if _, err := Parse(append([]byte{0xef, 0xbb, 0xbf}, []byte(podcastFixture)...)); err != nil {
		t.Fatalf("valid UTF-8 BOM rejected: %v", err)
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	f.Add([]byte(podcastFixture))
	f.Add([]byte(`{"version":"https://jsonfeed.org/version/1.1","title":"Test","items":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		_, _ = Parse(data)
	})
}
