package feed

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html/charset"
)

const (
	ContentNS = "http://purl.org/rss/1.0/modules/content/"
	MediaNS   = "http://search.yahoo.com/mrss/"
	ITunesNS  = "http://www.itunes.com/DTDs/PodCast-1.0.dtd"
	AtomNS    = "http://www.w3.org/2005/Atom"
	RSS1NS    = "http://purl.org/rss/1.0/"
	DCNS      = "http://purl.org/dc/elements/1.1/"
	PodcastNS = "https://podcastindex.org/namespace/1.0"
)

// Element preserves namespaced metadata, attributes and child order. This is
// semantic XML preservation, not a byte-for-byte round trip of the source.
type Element struct {
	Name  xml.Name   `json:"name"`
	Attrs []xml.Attr `json:"attrs,omitempty"`
	Parts []Part     `json:"parts,omitempty"`
}

type Part struct {
	Text  string   `json:"text,omitempty"`
	Child *Element `json:"child,omitempty"`
}

func (e Element) Text() string {
	var b strings.Builder
	for _, p := range e.Parts {
		if p.Child == nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (e Element) Attr(name string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}

func (e Element) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: e.Name}
	// Conventional prefixes improve compatibility with readers that do not
	// correctly handle a default namespace on extension elements. Declarations
	// live on the RSS root. Unknown namespaces receive local declarations so
	// they cannot accidentally change the namespace of unprefixed children.
	bindings := make(map[string]string)
	if prefix := namespacePrefix(e.Name.Space); prefix != "" {
		start.Name = xml.Name{Local: prefix + ":" + e.Name.Local}
	} else if e.Name.Space != "" {
		start.Name = xml.Name{Local: "ext:" + e.Name.Local}
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:ext"}, Value: e.Name.Space})
		bindings[e.Name.Space] = "ext"
	}
	for _, a := range e.Attrs {
		if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
			continue
		}
		if a.Name.Space != "" {
			prefix := namespacePrefix(a.Name.Space)
			if a.Name.Space == "http://www.w3.org/XML/1998/namespace" {
				prefix = "xml"
			}
			if prefix == "" {
				prefix = bindings[a.Name.Space]
				if prefix == "" {
					prefix = fmt.Sprintf("attr%d", len(bindings))
					bindings[a.Name.Space] = prefix
					start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: a.Name.Space})
				}
			}
			a.Name = xml.Name{Local: prefix + ":" + a.Name.Local}
		}
		start.Attr = append(start.Attr, a)
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, p := range e.Parts {
		if p.Child != nil {
			if err := enc.Encode(*p.Child); err != nil {
				return err
			}
		} else if err := enc.EncodeToken(xml.CharData(p.Text)); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

func readElement(d *xml.Decoder, start xml.StartElement, depth int, count *int) (Element, error) {
	(*count)++
	if depth > 64 || *count > 100000 {
		return Element{}, errors.New("XML nesting or element limit exceeded")
	}
	e := Element{Name: start.Name, Attrs: start.Attr}
	// Common broken spellings are repaired in generated extension metadata.
	if strings.EqualFold(e.Name.Space, ITunesNS) {
		e.Name.Space = ITunesNS
	}
	if e.Name.Space == strings.TrimSuffix(MediaNS, "/") {
		e.Name.Space = MediaNS
	}
	for {
		t, err := d.Token()
		if err != nil {
			return Element{}, err
		}
		switch t := t.(type) {
		case xml.StartElement:
			child, err := readElement(d, t, depth+1, count)
			if err != nil {
				return Element{}, err
			}
			e.Parts = append(e.Parts, Part{Child: &child})
		case xml.CharData:
			e.Parts = append(e.Parts, Part{Text: string(t)})
		case xml.EndElement:
			return e, nil
		case xml.Directive:
			return Element{}, errors.New("XML directives are not supported")
		}
	}
}

type Document struct {
	Feed       *gofeed.Feed `json:"feed"`
	Extras     []Element    `json:"extras,omitempty"`
	ItemExtras [][]Element  `json:"item_extras,omitempty"`
	ImageLink  string       `json:"image_link,omitempty"`
}

func Parse(data []byte) (*Document, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	// Enforce structural limits before entering the third-party XML parser.
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] != '{' {
		if err := validateXML(data); err != nil {
			return nil, err
		}
	}
	parser := gofeed.NewParser()
	f, err := parser.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	if len(f.Items) > 10000 {
		return nil, errors.New("feed exceeds 10000 items")
	}
	doc := &Document{Feed: f, ItemExtras: make([][]Element, len(f.Items))}
	// JSON Feed has no XML extension tree.
	if f.FeedType == "json" {
		return doc, nil
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.CharsetReader = charset.NewReaderLabel
	var root Element
	count := 0
	for {
		token, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("read XML: %w", err)
		}
		if _, ok := token.(xml.Directive); ok {
			return nil, errors.New("XML directives are not supported")
		}
		if start, ok := token.(xml.StartElement); ok {
			root, err = readElement(d, start, 0, &count)
			if err != nil {
				return nil, fmt.Errorf("read XML metadata: %w", err)
			}
			break
		}
	}
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
			if _, comment := token.(xml.Comment); !comment {
				return nil, errors.New("unexpected data after XML root")
			}
		}
	}
	if f.FeedType == "rss" {
		channel := &root
		if root.Name.Local == "rss" || root.Name.Local == "RDF" {
			for _, p := range root.Parts {
				if p.Child != nil && p.Child.Name.Local == "channel" {
					channel = p.Child
					break
				}
			}
		}
		index := 0
		for _, p := range channel.Parts {
			if p.Child == nil {
				continue
			}
			e := *p.Child
			if e.Name.Local == "item" && rssCoreNamespace(e.Name.Space) {
				if index < len(doc.ItemExtras) {
					doc.ItemExtras[index] = itemExtras(e)
					index++
				}
			} else if e.Name.Local == "image" && rssCoreNamespace(e.Name.Space) {
				for _, part := range e.Parts {
					if part.Child != nil && part.Child.Name.Local == "link" {
						doc.ImageLink = part.Child.Text()
					}
				}
			} else if channelExtra(e) {
				doc.Extras = append(doc.Extras, e)
			}
		}
		// RSS 1.0 places items beside the channel.
		if root.Name.Local == "RDF" {
			for _, p := range root.Parts {
				if p.Child != nil && p.Child.Name.Local == "item" && index < len(doc.ItemExtras) {
					doc.ItemExtras[index] = itemExtras(*p.Child)
					index++
				}
			}
		}
	} else if f.FeedType == "atom" {
		index := 0
		for _, p := range root.Parts {
			if p.Child == nil {
				continue
			}
			e := *p.Child
			if e.Name.Space == AtomNS || e.Name.Space == "" {
				if e.Name.Local == "entry" && index < len(doc.ItemExtras) {
					for _, part := range e.Parts {
						if part.Child != nil && part.Child.Name.Space != "" && part.Child.Name.Space != AtomNS {
							doc.ItemExtras[index] = append(doc.ItemExtras[index], *part.Child)
						}
					}
					index++
				}
				if (e.Name.Local == "logo" || e.Name.Local == "icon") &&
					(f.Image == nil || f.Image.URL == "" || e.Name.Local == "logo") {
					f.Image = &gofeed.Image{URL: strings.TrimSpace(e.Text()), Title: f.Title}
				}
			} else {
				doc.Extras = append(doc.Extras, e)
			}
		}
	}
	for index, extras := range doc.ItemExtras {
		if f.Items[index] == nil || f.Items[index].UpdatedParsed != nil {
			continue
		}
		for _, e := range extras {
			if e.Name.Space == AtomNS && e.Name.Local == "updated" {
				if updated, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(e.Text())); err == nil {
					f.Items[index].UpdatedParsed = &updated
				}
			}
		}
	}
	return doc, nil
}

func validateXML(data []byte) error {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.CharsetReader = charset.NewReaderLabel
	depth, elements, roots := 0, 0, 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			if roots != 1 || depth != 0 {
				return errors.New("source must contain one complete XML root")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid source XML: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			elements++
			if depth > 64 || elements > 100000 || roots > 1 {
				return errors.New("XML nesting, element or root limit exceeded")
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return errors.New("XML directives are not supported")
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return errors.New("unexpected text outside XML root")
			}
		}
	}
}

func channelExtra(e Element) bool {
	if !rssCoreNamespace(e.Name.Space) {
		// A source self/pagination link would point at the wrong feed.
		return !(e.Name.Space == AtomNS && e.Name.Local == "link")
	}
	switch e.Name.Local {
	case "title", "link", "description", "language", "copyright", "generator",
		"pubDate", "lastBuildDate", "image", "item", "items":
		return false
	}
	return true
}

func itemExtras(item Element) []Element {
	var extras []Element
	for _, p := range item.Parts {
		if p.Child == nil {
			continue
		}
		e := *p.Child
		if e.Name.Space == ContentNS && e.Name.Local == "encoded" {
			continue
		}
		if rssCoreNamespace(e.Name.Space) {
			switch e.Name.Local {
			case "title", "link", "description", "guid", "pubDate", "enclosure":
				continue
			}
		}
		extras = append(extras, e)
	}
	return extras
}

func rssCoreNamespace(namespace string) bool {
	return namespace == "" || namespace == RSS1NS || namespace == "http://my.netscape.com/rdf/simple/0.9/"
}

func namespacePrefix(namespace string) string {
	switch namespace {
	case ContentNS:
		return "content"
	case MediaNS:
		return "media"
	case ITunesNS:
		return "itunes"
	case AtomNS:
		return "atom"
	case DCNS:
		return "dc"
	case PodcastNS:
		return "podcast"
	}
	return ""
}
