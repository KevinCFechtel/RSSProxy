package feed

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length string `xml:"length,attr"`
}

type guidElement struct {
	Value       string `xml:",chardata"`
	IsPermalink string `xml:"isPermaLink,attr"`
}

type rssItem struct {
	Title       string      `xml:"title,omitempty"`
	Link        string      `xml:"link,omitempty"`
	Description string      `xml:"description,omitempty"`
	GUID        guidElement `xml:"guid"`
	Published   string      `xml:"pubDate,omitempty"`
	Enclosures  []enclosure `xml:"enclosure"`
	Extras      []Element   `xml:",any"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language,omitempty"`
	Copyright   string    `xml:"copyright,omitempty"`
	Generator   string    `xml:"generator"`
	Published   string    `xml:"pubDate,omitempty"`
	Updated     string    `xml:"lastBuildDate,omitempty"`
	Image       *rssImage `xml:"image,omitempty"`
	Extras      []Element `xml:",any"`
	Items       []rssItem `xml:"item"`
}

type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Content string     `xml:"xmlns:content,attr"`
	Media   string     `xml:"xmlns:media,attr"`
	ITunes  string     `xml:"xmlns:itunes,attr"`
	Atom    string     `xml:"xmlns:atom,attr"`
	DC      string     `xml:"xmlns:dc,attr"`
	Podcast string     `xml:"xmlns:podcast,attr"`
	Channel rssChannel `xml:"channel"`
}

func textElement(ns, name, value string) Element {
	return Element{Name: xml.Name{Space: ns, Local: name}, Parts: []Part{{Text: value}}}
}

func timeString(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC1123Z)
}

func resolve(base, value string) string {
	if value == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return value
	}
	u, err := url.Parse(value)
	if err != nil {
		return value
	}
	return b.ResolveReference(u).String()
}

// Render is deterministic: unchanged input/configuration yields identical bytes.
func Render(c Config, doc *Document, identities map[string]Identity) ([]byte, error) {
	if doc == nil || doc.Feed == nil {
		return nil, errors.New("missing parsed feed")
	}
	f := doc.Feed
	channel := rssChannel{
		Title:       c.Overrides.Title.Apply(f.Title),
		Description: c.Overrides.Description.Apply(f.Description),
		Link:        c.Overrides.Link.Apply(resolve(c.URL, f.Link)),
		Language:    f.Language, Copyright: f.Copyright, Generator: "RSSProxy",
		Published: timeString(f.PublishedParsed), Updated: timeString(f.UpdatedParsed),
		Extras: append([]Element(nil), doc.Extras...),
	}
	// Some publishers (for example Capital) provide an empty description.
	// Preserve it and still emit the required description element, rather than
	// blocking unrelated overrides or inventing metadata.
	if strings.TrimSpace(channel.Title) == "" || channel.Link == "" {
		return nil, errors.New("RSS requires channel title and website link; configure overrides for missing values")
	}
	if _, err := HTTPURL(channel.Link); err != nil {
		return nil, errors.New("channel website link is not an HTTP(S) URL")
	}
	if !hasElement(channel.Extras, "", "category") {
		for _, category := range f.Categories {
			channel.Extras = append(channel.Extras, textElement("", "category", category))
		}
	}
	if !hasElement(channel.Extras, "", "managingEditor") && !hasElement(channel.Extras, DCNS, "creator") {
		for _, author := range f.Authors {
			if author != nil && author.Name != "" {
				channel.Extras = append(channel.Extras, textElement(DCNS, "creator", author.Name))
			}
		}
	}
	imageURL, imageTitle := "", ""
	if f.Image != nil {
		imageURL, imageTitle = resolve(c.URL, f.Image.URL), f.Image.Title
	}
	imageURL = c.Overrides.ImageURL.Apply(imageURL)
	imageTitle = c.Overrides.ImageTitle.Apply(imageTitle)
	imageLink := c.Overrides.ImageLink.Apply(resolve(c.URL, doc.ImageLink))
	if imageURL != "" && c.Overrides.ImageTitle.Mode != "remove" && c.Overrides.ImageLink.Mode != "remove" {
		if imageTitle == "" {
			imageTitle = channel.Title
		}
		if imageLink == "" {
			imageLink = channel.Link
		}
		channel.Image = &rssImage{URL: imageURL, Title: imageTitle, Link: imageLink}
	}
	// Keep podcast readers consistent with the overridden channel artwork.
	if c.Overrides.ImageURL.Mode == "replace" || c.Overrides.ImageURL.Mode == "remove" ||
		c.Overrides.ImageTitle.Mode == "remove" || c.Overrides.ImageLink.Mode == "remove" {
		channel.Extras = filterArtwork(channel.Extras)
		if channel.Image != nil {
			channel.Extras = append(channel.Extras, artwork(ITunesNS, "image", imageURL))
		}
	}
	positions := make(map[string]int)
	for index, item := range f.Items {
		if item == nil {
			return nil, errors.New("source contains an empty item")
		}
		key, err := c.Identity.Key(item.GUID, item.Link)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", index+1, err)
		}
		identity, ok := identities[key]
		if !ok || identity.GUID == "" {
			return nil, fmt.Errorf("item %d has no persisted identity", index+1)
		}
		out := rssItem{
			Title: item.Title, Link: resolve(c.URL, item.Link), Description: item.Description,
			GUID:      guidElement{Value: identity.GUID, IsPermalink: "false"},
			Published: timeString(identity.Published),
		}
		if out.Title == "" && out.Description == "" && item.Content == "" {
			return nil, fmt.Errorf("item %d has no title or content", index+1)
		}
		if out.Title == "" && out.Description == "" {
			out.Description = item.Content
		}
		if index < len(doc.ItemExtras) {
			for _, e := range doc.ItemExtras[index] {
				if e.Name.Space != AtomNS || e.Name.Local != "updated" {
					out.Extras = append(out.Extras, e)
				}
			}
		}
		if item.Content != "" {
			out.Extras = append(out.Extras, textElement(ContentNS, "encoded", item.Content))
		}
		updated := identity.Updated
		if updated == nil {
			updated = item.UpdatedParsed
		}
		if updated != nil && !hasElement(out.Extras, AtomNS, "updated") {
			out.Extras = append(out.Extras, textElement(AtomNS, "updated", updated.UTC().Format(time.RFC3339Nano)))
		}
		if !hasElement(out.Extras, "", "category") {
			for _, category := range item.Categories {
				out.Extras = append(out.Extras, textElement("", "category", category))
			}
		}
		if !hasElement(out.Extras, "", "author") && !hasElement(out.Extras, DCNS, "creator") {
			for _, author := range item.Authors {
				if author != nil {
					out.Extras = append(out.Extras, textElement(DCNS, "creator", author.Name))
				}
			}
		}
		for _, e := range item.Enclosures {
			if e == nil || e.URL == "" || e.Type == "" {
				return nil, fmt.Errorf("item %d has an invalid enclosure", index+1)
			}
			if strings.HasPrefix(e.Type, "image/") &&
				(c.Overrides.ItemImage.Mode == "replace" || c.Overrides.ItemImage.Mode == "remove") {
				continue
			}
			length := e.Length
			if length == "" {
				length = "0" // unknown byte length; never substitute pixel width
			}
			n, err := strconv.ParseInt(length, 10, 64)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("item %d has an invalid enclosure byte length", index+1)
			}
			out.Enclosures = append(out.Enclosures, enclosure{URL: resolve(c.URL, e.URL), Type: e.Type, Length: length})
		}
		img := selectImage(c.Overrides.ItemImageSource, item, out.Extras)
		img = c.Overrides.ItemImage.Apply(resolve(c.URL, img))
		if c.Overrides.ItemImage.Mode == "replace" || c.Overrides.ItemImage.Mode == "remove" ||
			(c.Overrides.ItemImageSource != "" && c.Overrides.ItemImageSource != "auto") {
			out.Extras = filterArtwork(out.Extras)
		}
		if img != "" && c.Overrides.ItemImage.Mode != "remove" {
			if !hasArtwork(out.Extras, img) {
				out.Extras = append(out.Extras, artwork(MediaNS, "thumbnail", img))
			}
		}
		// Later occurrences of the same identity win, keeping first position.
		if pos, exists := positions[key]; exists {
			channel.Items[pos] = out
		} else {
			positions[key] = len(channel.Items)
			channel.Items = append(channel.Items, out)
		}
	}
	data, err := xml.Marshal(rssRoot{
		Version: "2.0", Content: ContentNS, Media: MediaNS, ITunes: ITunesNS,
		Atom: AtomNS, DC: DCNS, Podcast: PodcastNS, Channel: channel,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), data...), nil
}

func hasElement(elements []Element, ns, name string) bool {
	for _, e := range elements {
		if e.Name.Space == ns && e.Name.Local == name {
			return true
		}
	}
	return false
}

func artwork(ns, name, value string) Element {
	attr := "url"
	if ns == ITunesNS {
		attr = "href"
	}
	return Element{Name: xml.Name{Space: ns, Local: name}, Attrs: []xml.Attr{{Name: xml.Name{Local: attr}, Value: value}}}
}

func imageElement(e Element) bool {
	return (e.Name.Space == ITunesNS && e.Name.Local == "image") ||
		(e.Name.Space == MediaNS && (e.Name.Local == "thumbnail" ||
			(e.Name.Local == "content" && (strings.HasPrefix(e.Attr("type"), "image/") || e.Attr("medium") == "image"))))
}

func filterArtwork(elements []Element) []Element {
	out := make([]Element, 0, len(elements))
	for _, e := range elements {
		if imageElement(e) {
			continue
		}
		// Remove nested media artwork too, without deleting audio/video metadata.
		parts := make([]Part, 0, len(e.Parts))
		for _, part := range e.Parts {
			if part.Child != nil {
				children := filterArtwork([]Element{*part.Child})
				if len(children) == 0 {
					continue
				}
				child := children[0]
				part.Child = &child
			}
			parts = append(parts, part)
		}
		e.Parts = parts
		out = append(out, e)
	}
	return out
}

func hasArtwork(elements []Element, value string) bool {
	for _, e := range elements {
		if imageElement(e) && (e.Attr("url") == value || e.Attr("href") == value) {
			return true
		}
	}
	return false
}

func selectImage(source string, item *gofeed.Item, extras []Element) string {
	if (source == "" || source == "auto" || source == "image") && item.Image != nil && item.Image.URL != "" {
		return item.Image.URL
	}
	if source == "enclosure" {
		for _, e := range item.Enclosures {
			if e != nil && strings.HasPrefix(e.Type, "image/") {
				return e.URL
			}
		}
		return ""
	}
	if source == "image" {
		return ""
	}
	ns, name := "", ""
	if prefix, local, ok := strings.Cut(source, ":"); ok {
		name = local
		switch prefix {
		case "media":
			ns = MediaNS
		case "itunes":
			ns = ITunesNS
		default:
			// gofeed's extension map retains source prefixes for custom rules.
			for _, e := range item.Extensions[prefix][local] {
				if e.Attrs["url"] != "" {
					return e.Attrs["url"]
				}
				if e.Attrs["href"] != "" {
					return e.Attrs["href"]
				}
			}
			return ""
		}
	}
	var find func([]Element) string
	find = func(elements []Element) string {
		for _, e := range elements {
			matches := imageElement(e)
			if ns != "" {
				matches = e.Name.Space == ns && e.Name.Local == name
			}
			if matches {
				if value := e.Attr("url"); value != "" {
					return value
				}
				if value := e.Attr("href"); value != "" {
					return value
				}
			}
			for _, p := range e.Parts {
				if p.Child != nil {
					if value := find([]Element{*p.Child}); value != "" {
						return value
					}
				}
			}
		}
		return ""
	}
	if value := find(extras); value != "" {
		return value
	}
	if source == "" || source == "auto" {
		for _, e := range item.Enclosures {
			if e != nil && strings.HasPrefix(e.Type, "image/") {
				return e.URL
			}
		}
	}
	return ""
}
