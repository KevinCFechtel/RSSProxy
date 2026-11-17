package feed

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Key never silently falls back from a failed extraction to the changing GUID.
// URL rules operate on paths, not on query strings or arbitrary hostnames.
func (r IdentityRule) Key(guid, link string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	input := guid
	if r.Source == "link" {
		input = link
	}
	if strings.TrimSpace(input) == "" {
		return "", errors.New("identity source is empty; configure a different strategy")
	}
	if len(input) > 16384 {
		return "", errors.New("identity source is too long")
	}
	var u *url.URL
	if parsed, err := HTTPURL(input); err == nil {
		u = parsed
	}
	if len(r.Hosts) > 0 {
		if u == nil {
			return "", errors.New("identity rule requires an HTTP(S) URL")
		}
		matched := false
		for _, host := range r.Hosts {
			matched = matched || strings.EqualFold(u.Hostname(), host)
		}
		if !matched {
			return "", fmt.Errorf("identity URL host is not allowed")
		}
	}
	key := input
	switch r.Kind {
	case "regex":
		if u != nil {
			input = u.Path
		}
		re := regexp.MustCompile(r.Regex) // validated above
		matches := re.FindStringSubmatch(input)
		if matches == nil {
			return "", errors.New("identity regex did not match")
		}
		key = matches[re.SubexpIndex("id")]
	case "query":
		if u == nil {
			return "", errors.New("query identity requires an HTTP(S) URL")
		}
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(values[r.Query]) != 1 {
			return "", errors.New("identity query parameter is missing, repeated, or invalid")
		}
		key = values.Get(r.Query)
	case "legacy":
		start := r.LegacyOffset
		if r.LegacyDirection == "End" {
			start = len(input) - r.LegacyOffset
		}
		if start < 0 || start > len(input) || r.LegacyLength > len(input)-start {
			return "", errors.New("legacy identity slice is outside the GUID")
		}
		key = input[start : start+r.LegacyLength]
	}
	if strings.TrimSpace(key) == "" {
		return "", errors.New("extracted identity is empty")
	}
	return key, nil
}
