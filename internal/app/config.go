package app

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr        string
	DatabaseURL       string
	LegacyDatabaseURL string
	AuthUser          string
	AuthPassword      string
	CookieSecure      bool
	SourceTimeout     time.Duration
	RequestTimeout    time.Duration
	MaxFeedBytes      int64
	AllowedNetworks   []netip.Prefix
}

func secret(name string) (string, error) {
	value, file := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && file != "" {
		return "", fmt.Errorf("set either %s or %s_FILE, not both", name, name)
	}
	if file != "" {
		bytes, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE failed", name)
		}
		value = strings.TrimRight(string(bytes), "\r\n")
	}
	return value, nil
}

func Load() (Config, error) {
	c := Config{ListenAddr: ":8080", SourceTimeout: 20 * time.Second,
		RequestTimeout: 60 * time.Second, MaxFeedBytes: 10 << 20}
	var err error
	for _, setting := range []struct {
		name string
		dest *string
	}{
		{"DATABASE_URL", &c.DatabaseURL}, {"LEGACY_DATABASE_URL", &c.LegacyDatabaseURL},
		{"AUTH_USER", &c.AuthUser}, {"AUTH_PASSWORD", &c.AuthPassword},
	} {
		*setting.dest, err = secret(setting.name)
		if err != nil {
			return c, err
		}
	}
	if c.DatabaseURL == "" || c.AuthUser == "" || c.AuthPassword == "" {
		return c, errors.New("DATABASE_URL, AUTH_USER and AUTH_PASSWORD are required (or their _FILE variants)")
	}
	if value := os.Getenv("LISTEN_ADDR"); value != "" {
		c.ListenAddr = value
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return c, errors.New("LISTEN_ADDR must use host:port syntax, for example :8080")
	}
	for _, setting := range []struct {
		name string
		dest *time.Duration
	}{{"SOURCE_TIMEOUT", &c.SourceTimeout}, {"REQUEST_TIMEOUT", &c.RequestTimeout}} {
		if value := os.Getenv(setting.name); value != "" {
			*setting.dest, err = time.ParseDuration(value)
			if err != nil || *setting.dest < time.Second || *setting.dest > 5*time.Minute {
				return c, fmt.Errorf("%s must be between 1s and 5m", setting.name)
			}
		}
	}
	if c.RequestTimeout < c.SourceTimeout {
		return c, errors.New("REQUEST_TIMEOUT must be at least SOURCE_TIMEOUT")
	}
	if value := os.Getenv("MAX_FEED_BYTES"); value != "" {
		c.MaxFeedBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.MaxFeedBytes < 1024 || c.MaxFeedBytes > 100<<20 {
			return c, errors.New("MAX_FEED_BYTES must be between 1024 and 104857600")
		}
	}
	if value := os.Getenv("COOKIE_SECURE"); value != "" {
		c.CookieSecure, err = strconv.ParseBool(value)
		if err != nil {
			return c, errors.New("COOKIE_SECURE must be true or false")
		}
	}
	for _, value := range strings.FieldsFunc(os.Getenv("ALLOWED_NETWORKS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n'
	}) {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return c, errors.New("ALLOWED_NETWORKS must contain explicit IPv4/IPv6 CIDRs")
		}
		c.AllowedNetworks = append(c.AllowedNetworks, prefix.Masked())
	}
	return c, nil
}
