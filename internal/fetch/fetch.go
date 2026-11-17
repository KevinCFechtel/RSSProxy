// Package fetch owns outbound HTTP, including connection-time SSRF protection.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
)

type Error struct {
	Status     int
	Message    string
	RetryAfter string
}

func (e *Error) Error() string { return e.Message }

type Validators struct {
	ETag         string
	LastModified string
}

type Result struct {
	Body        []byte
	NotModified bool
	Validators  Validators
}

type Fetcher struct {
	Client   *http.Client
	MaxBytes int64
}

// Explicit CIDR allowances also make local/self-hosted feeds possible without
// weakening the policy for all other destinations.
func New(timeout time.Duration, maxBytes int64, allowed []netip.Prefix) *Fetcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:             nil, // environment proxies could bypass destination validation
		ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 5,
		MaxConnsPerHost: 20, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout,
		MaxResponseHeaderBytes: 1 << 20,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("invalid source address")
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, errors.New("source DNS lookup failed")
			}
			if len(ips) == 0 {
				return nil, errors.New("source has no IP addresses")
			}
			// Reject mixed public/private DNS answers as well.
			for _, ip := range ips {
				if !Allowed(ip, allowed) {
					return nil, errors.New("source destination is blocked by network policy")
				}
			}
			for _, ip := range ips {
				// Dial the already validated address, never do a second DNS lookup.
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, errors.New("source connection failed")
		},
	}
	return &Fetcher{
		MaxBytes: maxBytes,
		Client: &http.Client{
			Timeout: timeout, Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many source redirects")
				}
				if _, err := feed.HTTPURL(req.URL.String()); err != nil {
					return err
				}
				if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
					return errors.New("HTTPS downgrade redirect is blocked")
				}
				return nil
			},
		},
	}
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("fec0::/10"),
}

func Allowed(ip netip.Addr, allow []netip.Prefix) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	for _, prefix := range allow {
		if prefix.Contains(ip) {
			return true
		}
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (f *Fetcher) Get(ctx context.Context, rawURL string, validators Validators) (Result, error) {
	if _, err := feed.HTTPURL(rawURL); err != nil {
		return Result{}, &Error{Status: http.StatusBadGateway, Message: "Invalid source URL"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, &Error{Status: http.StatusBadGateway, Message: "Invalid source request"}
	}
	req.Header.Set("User-Agent", "RSSProxy/1.0")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/feed+json, application/xml, text/xml;q=0.9")
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		status, message := http.StatusBadGateway, "Source request failed (connection, redirect or network policy)"
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			status, message = http.StatusGatewayTimeout, "Source request timed out"
		}
		return Result{}, &Error{Status: status, Message: message}
	}
	defer resp.Body.Close()
	result := Result{Validators: Validators{
		ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
	}}
	if resp.StatusCode == http.StatusNotModified {
		result.NotModified = true
		if result.Validators.ETag == "" {
			result.Validators.ETag = validators.ETag
		}
		if result.Validators.LastModified == "" {
			result.Validators.LastModified = validators.LastModified
		}
		return result, nil
	}
	if resp.StatusCode != http.StatusOK {
		status := resp.StatusCode
		if status < 400 || status > 599 || status == http.StatusUnauthorized || status == http.StatusProxyAuthRequired {
			status = http.StatusBadGateway
		}
		retry := resp.Header.Get("Retry-After")
		if !validRetryAfter(retry) {
			retry = ""
		}
		return Result{}, &Error{Status: status, Message: fmt.Sprintf("Source returned HTTP %d", resp.StatusCode), RetryAfter: retry}
	}
	// Go's transport transparently decompresses gzip. Limit the decoded body,
	// not Content-Length, to also bound compressed payload expansion.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		status := http.StatusBadGateway
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			status = http.StatusGatewayTimeout
		}
		return Result{}, &Error{Status: status, Message: "Reading source response failed"}
	}
	if int64(len(body)) > f.MaxBytes {
		return Result{}, &Error{Status: http.StatusBadGateway, Message: "Source feed exceeds the configured size limit"}
	}
	result.Body = body
	return result, nil
}

func validRetryAfter(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	if _, err := http.ParseTime(value); err == nil {
		return true
	}
	return strings.Trim(value, "0123456789") == ""
}
