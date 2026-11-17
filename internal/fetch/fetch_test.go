package fetch

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNetworkPolicy(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "::ffff:169.254.169.254", "fd00:ec2::254", "fe80::1",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::"} {
		if Allowed(netip.MustParseAddr(value), nil) {
			t.Errorf("unsafe destination allowed: %s", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !Allowed(netip.MustParseAddr(value), nil) {
			t.Errorf("public destination blocked: %s", value)
		}
	}
	if !Allowed(netip.MustParseAddr("127.0.0.1"), []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}) {
		t.Fatal("explicit network allowance ignored")
	}
}

func localFetcher(limit int64) *Fetcher {
	return New(time.Second, limit, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
}

func TestEveryGetContactsSourceAndValidates(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			if r.Header.Get("If-None-Match") != `"v1"` || r.Header.Get("If-Modified-Since") == "" {
				t.Error("conditional headers missing")
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Mon, 16 Nov 2026 12:00:00 GMT")
		_, _ = w.Write([]byte("feed"))
	}))
	defer server.Close()
	f := localFetcher(1024)
	first, err := f.Get(context.Background(), server.URL, Validators{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Get(context.Background(), server.URL, first.Validators)
	if err != nil || !second.NotModified || calls != 2 {
		t.Fatalf("source not contacted: result=%+v calls=%d err=%v", second, calls, err)
	}
}

func TestBlockedLocalSourceAndRedirect(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()
	if _, err := New(time.Second, 1024, nil).Get(context.Background(), server.URL, Validators{}); err == nil {
		t.Fatal("localhost accepted without allowance")
	}
	if _, err := localFetcher(1024).Get(context.Background(), server.URL, Validators{}); err == nil {
		t.Fatal("redirect bypassed network policy")
	}
}

func TestCompressedBodyLimitAndHTTPFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(strings.Repeat("x", 4096)))
			_ = gz.Close()
		case "/429":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/slow":
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	f := localFetcher(1024)
	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/gzip", 502}, {"/429", 429}, {"/503", 503}, {"/slow", 504},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := f.Get(ctx, server.URL+tt.path, Validators{})
		cancel()
		var upstream *Error
		if !errors.As(err, &upstream) || upstream.Status != tt.status {
			t.Errorf("%s: got %v, want status %d", tt.path, err, tt.status)
		}
		if tt.path == "/429" && upstream != nil && upstream.RetryAfter != "120" {
			t.Error("Retry-After not preserved")
		}
	}
}
