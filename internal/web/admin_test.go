package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
)

func TestEnglishTemplates(t *testing.T) {
	t.Parallel()
	admin, err := NewAdmin(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "admin", "password", false)
	if err != nil {
		t.Fatal(err)
	}
	config := feed.DefaultConfig()
	config.Name, config.Path, config.URL = "Example", "/example", "https://example.org/rss"
	config.Identity.Kind = "legacy"
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, template string
		page           page
		want           []string
	}{
		{
			name: "empty list", template: "list", page: page{Title: "Feeds"},
			want: []string{"Add feed", "No feeds configured yet."},
		},
		{
			name: "feed list", template: "list",
			page: page{Title: "Feeds", Feeds: []store.Summary{
				{Config: config, State: store.State{LastAttempt: &now, LastSuccess: &now, LastError: "Source timeout"}},
				{Config: config},
			}},
			want: []string{"Endpoint", "Source", "Identity", "Imported truncation rule",
				"Last attempt", "Last success", "2026-11-19 12:00:00 UTC", "Not fetched yet",
				"Error", "Edit / Preview", "Permanently delete feed"},
		},
		{
			name: "form", template: "form",
			page: page{Title: "Configure feed", Config: config, Preview: []preview{{Key: "123"}}},
			want: []string{"Configure feed", "Name (internal)", "Public endpoint", "Source URL",
				"Article identity", "Extraction source", "Overrides", "Feed title", "Feed description",
				"Website link", "Feed image / Logo URL", "Image title", "Image link", "Article image URL",
				"Keep original", "Always replace", "Fill in only if missing", "Remove",
				"Rule preview without saving", "Test extraction", "Extracted ID / Error", "<code>123</code>",
				">Save</button>", ">Cancel</a>"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
			admin.render(w, r, tt.template, tt.page, http.StatusOK)
			body := w.Body.String()
			for _, want := range append(tt.want, `<html lang="en">`, "RSS with stable article identities",
				"Every feed request contacts the source.", "</html>") {
				if !strings.Contains(body, want) {
					t.Errorf("rendered page missing %q", want)
				}
			}
		})
	}
}
