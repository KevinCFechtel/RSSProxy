package feed

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Override deliberately separates an empty value from the decision to remove it.
type Override struct {
	Mode  string `json:"mode"`
	Value string `json:"value"`
}

func (o Override) Apply(original string) string {
	switch o.Mode {
	case "replace":
		return o.Value
	case "fallback":
		if strings.TrimSpace(original) == "" {
			return o.Value
		}
	case "remove":
		return ""
	}
	return original
}

type IdentityRule struct {
	Source string   `json:"source"` // guid or link
	Kind   string   `json:"kind"`   // full, regex, query, or legacy
	Regex  string   `json:"regex"`
	Query  string   `json:"query"`
	Hosts  []string `json:"hosts,omitempty"`
	// Legacy fields preserve the old byte-slicing semantics for imported feeds.
	LegacyDirection string `json:"legacy_direction,omitempty"`
	LegacyOffset    int    `json:"legacy_offset,omitempty"`
	LegacyLength    int    `json:"legacy_length,omitempty"`
}

type Overrides struct {
	Title       Override `json:"title"`
	Description Override `json:"description"`
	Link        Override `json:"link"`
	ImageURL    Override `json:"image_url"`
	ImageTitle  Override `json:"image_title"`
	ImageLink   Override `json:"image_link"`
	ItemImage   Override `json:"item_image"`
	// auto, image, enclosure, or namespace:element (for example media:content).
	ItemImageSource string `json:"item_image_source"`
}

type Config struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	Path      string       `json:"path"`
	URL       string       `json:"url"`
	Identity  IdentityRule `json:"identity"`
	Overrides Overrides    `json:"overrides"`
	Revision  int64        `json:"revision"`
}

func DefaultConfig() Config {
	return Config{
		Identity:  IdentityRule{Source: "guid", Kind: "full"},
		Overrides: Overrides{ItemImageSource: "auto"},
	}
}

func HTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("expected an absolute HTTP(S) URL without credentials")
	}
	return u, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 200 {
		return errors.New("name must contain 1–200 characters")
	}
	if len(c.URL) > 4096 {
		return errors.New("source URL is too long")
	}
	if _, err := HTTPURL(c.URL); err != nil {
		return fmt.Errorf("source URL: %w", err)
	}
	u, err := url.ParseRequestURI(c.Path)
	if err != nil || u.Path != c.Path || u.RawQuery != "" || u.Host != "" ||
		!strings.HasPrefix(c.Path, "/") || strings.Contains(c.Path, "//") ||
		strings.Contains(c.Path, "%") || strings.ContainsAny(c.Path, "{} \t\r\n") ||
		c.Path == "/" || len(c.Path) > 500 {
		return errors.New("endpoint must be an absolute URL path without query, escaping, or router wildcards")
	}
	for _, part := range strings.Split(c.Path, "/") {
		if part == "." || part == ".." {
			return errors.New("endpoint must not contain dot segments")
		}
	}
	for _, prefix := range []string{"/admin", "/configs", "/healthz", "/readyz", "/favicon.ico"} {
		if c.Path == prefix || strings.HasPrefix(c.Path, prefix+"/") {
			return fmt.Errorf("endpoint prefix %s is reserved", prefix)
		}
	}
	if err := c.Identity.Validate(); err != nil {
		return err
	}
	fields := []struct {
		name     string
		o        Override
		required bool
		url      bool
	}{
		{"title", c.Overrides.Title, true, false},
		{"description", c.Overrides.Description, true, false},
		{"link", c.Overrides.Link, true, true},
		{"image URL", c.Overrides.ImageURL, false, true},
		{"image title", c.Overrides.ImageTitle, false, false},
		{"image link", c.Overrides.ImageLink, false, true},
		{"item image", c.Overrides.ItemImage, false, true},
	}
	for _, f := range fields {
		if len(f.o.Value) > 8192 {
			return fmt.Errorf("%s override is too long", f.name)
		}
		switch f.o.Mode {
		case "", "original":
		case "remove":
			if f.required {
				return fmt.Errorf("%s is required by RSS and cannot be removed", f.name)
			}
		case "replace", "fallback":
			if strings.TrimSpace(f.o.Value) == "" {
				return fmt.Errorf("%s override needs a value", f.name)
			}
			if f.url {
				if _, err := HTTPURL(f.o.Value); err != nil {
					return fmt.Errorf("%s: %w", f.name, err)
				}
			}
		default:
			return fmt.Errorf("invalid override mode for %s", f.name)
		}
	}
	source := c.Overrides.ItemImageSource
	if source != "" && source != "auto" && source != "image" && source != "enclosure" &&
		!regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*:[A-Za-z_][A-Za-z0-9_-]*$`).MatchString(source) {
		return errors.New("image source must be auto, image, enclosure, or namespace:element")
	}
	return nil
}

func (r IdentityRule) Validate() error {
	if r.Source != "guid" && r.Source != "link" {
		return errors.New("identity source must be guid or link")
	}
	switch r.Kind {
	case "full":
	case "regex":
		if len(r.Regex) > 2048 {
			return errors.New("identity regex is too long")
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return fmt.Errorf("identity regex: %w", err)
		}
		if re.SubexpIndex("id") < 1 {
			return errors.New("identity regex needs a named capture group (?P<id>...)")
		}
	case "query":
		if r.Query == "" || len(r.Query) > 200 {
			return errors.New("query identity requires a parameter name")
		}
	case "legacy":
		if (r.LegacyDirection != "Start" && r.LegacyDirection != "End") ||
			r.LegacyOffset < 0 || r.LegacyLength <= 0 {
			return errors.New("invalid legacy identity offsets; review the imported rule")
		}
	default:
		return errors.New("unknown identity strategy")
	}
	for _, host := range r.Hosts {
		if host == "" || strings.ContainsAny(host, "/:@ \t\r\n") {
			return errors.New("identity hosts must be hostnames without scheme, port, or path")
		}
	}
	return nil
}

type Identity struct {
	Key       string     `json:"key"`
	GUID      string     `json:"guid"`
	Published *time.Time `json:"published,omitempty"`
	Updated   *time.Time `json:"updated,omitempty"`
}
