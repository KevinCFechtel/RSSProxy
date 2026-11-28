package feed

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// GroupSource includes the original source URL for resolving relative links,
// enclosures and images when the merged feed is rendered.
type GroupSource struct {
	URL      string
	Document *Document
}

type groupItem struct {
	item   *gofeed.Item
	extras []Element
	base   string
}

// MergeGroup combines current snapshots using the group's identity rule.
// The first source wins duplicate article metadata; categories are combined.
// Source documents are not modified, and missing articles are not resurrected.
func MergeGroup(c Config, sources []GroupSource) (*Document, error) {
	if c.Type != "group" || len(sources) == 0 || len(sources) > 8 {
		return nil, errors.New("invalid group sources")
	}
	first := sources[0]
	if first.Document == nil || first.Document.Feed == nil {
		return nil, errors.New("missing group source document")
	}
	channel := *first.Document.Feed
	channel.Title = c.Name
	channel.Description = "Articles from grouped RSS sources"
	channel.Items = nil
	channel.Categories = nil
	channel.PublishedParsed = nil
	channel.UpdatedParsed = nil
	result := &Document{Feed: &channel, BaseURL: first.URL, ImageLink: first.Document.ImageLink}
	seen := make(map[string]int)
	var items []groupItem
	for _, source := range sources {
		if source.Document == nil || source.Document.Feed == nil {
			return nil, errors.New("missing group source document")
		}
		for i, original := range source.Document.Feed.Items {
			if original == nil {
				return nil, errors.New("group source contains an empty item")
			}
			item := *original
			item.Link = resolve(source.URL, item.Link)
			item.Categories = append([]string(nil), item.Categories...)
			key, err := c.Identity.Key(item.GUID, item.Link)
			if err != nil {
				return nil, fmt.Errorf("group article identity: %w", err)
			}
			var extras []Element
			if i < len(source.Document.ItemExtras) {
				extras = resolveGroupExtras(source.URL, source.Document.ItemExtras[i])
			}
			if pos, ok := seen[key]; ok {
				for _, category := range item.Categories {
					if !slices.Contains(items[pos].item.Categories, category) {
						items[pos].item.Categories = append(items[pos].item.Categories, category)
					}
				}
				for _, category := range extras {
					if !isItemCategory(category) {
						continue
					}
					found := false
					for _, existing := range items[pos].extras {
						if isItemCategory(existing) && strings.TrimSpace(existing.Text()) == strings.TrimSpace(category.Text()) &&
							existing.Attr("domain") == category.Attr("domain") {
							found = true
							break
						}
					}
					if !found {
						items[pos].extras = append(items[pos].extras, category)
					}
				}
				continue
			}
			if len(items) >= 10000 {
				return nil, errors.New("merged group exceeds 10000 unique articles")
			}
			seen[key] = len(items)
			items = append(items, groupItem{item: &item, extras: extras, base: source.URL})
		}
	}
	// Keep original category domains, adding categories from Atom/JSON input.
	for i := range items {
		for _, category := range items[i].item.Categories {
			found := false
			for _, e := range items[i].extras {
				if isItemCategory(e) && strings.TrimSpace(e.Text()) == category {
					found = true
					break
				}
			}
			if !found {
				items[i].extras = append(items[i].extras, textElement("", "category", category))
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := groupItemDate(items[i].item), groupItemDate(items[j].item)
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		return left.After(*right)
	})
	for _, item := range items {
		result.Feed.Items = append(result.Feed.Items, item.item)
		result.ItemExtras = append(result.ItemExtras, item.extras)
		result.ItemSourceURLs = append(result.ItemSourceURLs, item.base)
	}
	return result, nil
}

func isItemCategory(e Element) bool {
	return rssCoreNamespace(e.Name.Space) && e.Name.Local == "category"
}

// Clone extension trees before resolving known URL fields. In particular,
// retained media artwork must not point at the group's endpoint.
func resolveGroupExtras(base string, elements []Element) []Element {
	result := make([]Element, 0, len(elements))
	for _, e := range elements {
		e.Attrs = slices.Clone(e.Attrs)
		if e.Name.Space == MediaNS || e.Name.Space == ITunesNS ||
			e.Name.Space == PodcastNS || e.Name.Space == AtomNS ||
			(rssCoreNamespace(e.Name.Space) && e.Name.Local == "source") {
			for i := range e.Attrs {
				if e.Attrs[i].Name.Space == "" &&
					(e.Attrs[i].Name.Local == "url" || e.Attrs[i].Name.Local == "href") {
					e.Attrs[i].Value = resolve(base, e.Attrs[i].Value)
				}
			}
		}
		e.Parts = slices.Clone(e.Parts)
		for i := range e.Parts {
			if e.Parts[i].Child != nil {
				child := resolveGroupExtras(base, []Element{*e.Parts[i].Child})[0]
				e.Parts[i].Child = &child
			} else if rssCoreNamespace(e.Name.Space) && e.Name.Local == "comments" {
				e.Parts[i].Text = resolve(base, e.Parts[i].Text)
			}
		}
		result = append(result, e)
	}
	return result
}

func groupItemDate(item *gofeed.Item) *time.Time {
	if item.PublishedParsed != nil {
		return item.PublishedParsed
	}
	return item.UpdatedParsed
}
