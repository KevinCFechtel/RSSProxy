package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"DATABASE_URL", "DATABASE_URL_FILE", "AUTH_USER", "AUTH_USER_FILE",
		"AUTH_PASSWORD", "AUTH_PASSWORD_FILE", "LEGACY_DATABASE_URL", "LEGACY_DATABASE_URL_FILE",
		"LISTEN_ADDR", "SOURCE_TIMEOUT", "REQUEST_TIMEOUT", "MAX_FEED_BYTES", "COOKIE_SECURE", "ALLOWED_NETWORKS",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://example/database")
	t.Setenv("AUTH_USER", "admin")
	t.Setenv("AUTH_PASSWORD", "password")
}

func TestLoadDefaultsAndSecretFiles(t *testing.T) {
	validEnvironment(t)
	c, err := Load()
	if err != nil || c.ListenAddr != ":8080" || c.SourceTimeout != 20*time.Second || c.MaxFeedBytes != 10<<20 {
		t.Fatalf("bad defaults: %+v %v", c, err)
	}
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("file-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_PASSWORD", "")
	t.Setenv("AUTH_PASSWORD_FILE", path)
	t.Setenv("COOKIE_SECURE", "true")
	t.Setenv("ALLOWED_NETWORKS", "127.0.0.1/32,192.168.1.0/24")
	c, err = Load()
	if err != nil || c.AuthPassword != "file-password" || !c.CookieSecure || len(c.AllowedNetworks) != 2 {
		t.Fatalf("secret file/network settings failed: %+v %v", c, err)
	}
	t.Setenv("AUTH_PASSWORD", "conflicting-value")
	if _, err := Load(); err == nil {
		t.Fatal("ambiguous secret settings accepted")
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"DATABASE_URL", ""}, {"AUTH_USER", ""}, {"AUTH_PASSWORD", ""},
		{"LISTEN_ADDR", "8080"}, {"SOURCE_TIMEOUT", "0s"}, {"SOURCE_TIMEOUT", "invalid"},
		{"REQUEST_TIMEOUT", "1s"}, {"MAX_FEED_BYTES", "-1"}, {"MAX_FEED_BYTES", "999999999999"},
		{"COOKIE_SECURE", "maybe"}, {"ALLOWED_NETWORKS", "localhost"},
	} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil {
				t.Fatalf("invalid %s=%q accepted", tt.key, tt.value)
			}
		})
	}
}
