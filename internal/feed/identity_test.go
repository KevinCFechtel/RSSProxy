package feed

import (
	"strings"
	"testing"
)

func TestIdentityRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, guid, link, want string
		rule                   IdentityRule
	}{
		{"FAZ", "https://www.faz.net/aktuell/sport/fussball/alte-ueberschrift-accg-201686703.html?tracking=123", "", "201686703",
			IdentityRule{Source: "guid", Kind: "regex", Regex: `-(?P<id>[0-9]+)\.html$`, Hosts: []string{"www.faz.net"}}},
		{"FAZ updated", "https://www.faz.net/andere-rubrik/neue-ueberschrift-accg-201686703.html", "", "201686703",
			IdentityRule{Source: "guid", Kind: "regex", Regex: `-(?P<id>[0-9]+)\.html$`}},
		{"Handelsblatt", "", "https://www.handelsblatt.com/finanzen/banken-versicherungen/banken/banken-eu-und-fed-erlauben-commerzbank-uebernahme-durch-unicredit/100270206.html", "100270206",
			IdentityRule{Source: "link", Kind: "regex", Regex: `/(?P<id>[0-9]+)\.html$`}},
		{"query", "https://example.org/article?articleId=123&title=new", "", "123",
			IdentityRule{Source: "guid", Kind: "query", Query: "articleId"}},
		{"opaque GUID", "opaque:123", "", "opaque:123", IdentityRule{Source: "guid", Kind: "full"}},
		{"legacy end", "prefix-123-tail", "", "123",
			IdentityRule{Source: "guid", Kind: "legacy", LegacyDirection: "End", LegacyOffset: 8, LegacyLength: 3}},
		{"legacy start", "prefix-123-tail", "", "123",
			IdentityRule{Source: "guid", Kind: "legacy", LegacyDirection: "Start", LegacyOffset: 7, LegacyLength: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.rule.Key(tt.guid, tt.link)
			if err != nil || got != tt.want {
				t.Fatalf("Key() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestIdentityFailuresNeverFallback(t *testing.T) {
	t.Parallel()
	tests := []IdentityRule{
		{Source: "guid", Kind: "regex", Regex: `(?P<id>[0-9]+)\.html$`, Hosts: []string{"other.example"}},
		{Source: "guid", Kind: "regex", Regex: `(?P<id>[0-9]+)\.html$`},
		{Source: "guid", Kind: "regex", Regex: `([0-9]+)`},
		{Source: "guid", Kind: "regex", Regex: `(?P<id>.*)`},
		{Source: "guid", Kind: "query", Query: "missing"},
		{Source: "guid", Kind: "legacy", LegacyDirection: "End", LegacyOffset: 1000, LegacyLength: 5},
		{Source: "guid", Kind: "legacy", LegacyDirection: "Start", LegacyOffset: -1, LegacyLength: 5},
	}
	for i, rule := range tests {
		input := "https://example.org/not-an-id"
		if i == 3 {
			input = ""
		}
		if _, err := rule.Key(input, ""); err == nil {
			t.Errorf("rule %d accepted invalid input", i)
		}
	}
	rule := IdentityRule{Source: "guid", Kind: "query", Query: "id"}
	if _, err := rule.Key("https://example.org/?id=1&id=2", ""); err == nil {
		t.Fatal("repeated identity query accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Name, c.Path, c.URL = "Example", "/feeds/example", "https://example.org/rss"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/admin", "/admin/anything", "/healthz", "/feeds/{id}", "/feeds/../other", "//example.org", "/feed?q=1", "/feed%20a"} {
		c.Path = path
		if err := c.Validate(); err == nil {
			t.Errorf("invalid path %q accepted", path)
		}
	}
	c.Path = "/feeds/example"
	c.Overrides.Title = Override{Mode: "remove"}
	if err := c.Validate(); err == nil {
		t.Fatal("required RSS title may not be removed")
	}
	c.Overrides.Title = Override{}
	c.Overrides.ImageURL = Override{Mode: "replace", Value: "javascript:alert(1)"}
	if err := c.Validate(); err == nil {
		t.Fatal("unsafe image override accepted")
	}
}

func FuzzIdentityNeverPanics(f *testing.F) {
	f.Add("https://example.org/a-123.html", 3, 3, true)
	f.Add("", -1, -1, false)
	f.Fuzz(func(t *testing.T, input string, offset, length int, fromEnd bool) {
		if len(input) > 20000 {
			t.Skip()
		}
		direction := "Start"
		if fromEnd {
			direction = "End"
		}
		r := IdentityRule{Source: "guid", Kind: "legacy", LegacyDirection: direction, LegacyOffset: offset, LegacyLength: length}
		key, err := r.Key(input, "")
		if err == nil && strings.TrimSpace(key) == "" {
			t.Fatal("empty identity accepted")
		}
	})
}
