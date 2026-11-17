package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/feed"
	"github.com/KevinCFechtel/RSSProxy/internal/fetch"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
)

type Source interface {
	Get(context.Context, string, fetch.Validators) (fetch.Result, error)
}

type Repository interface {
	ByPath(context.Context, string) (feed.Config, error)
	Lock(context.Context, int64) (*store.Locked, error)
}

type Service struct {
	Store   Repository
	Source  Source
	Logger  *slog.Logger
	Timeout time.Duration
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	c, err := s.Store.ByPath(ctx, r.URL.Path)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, 0, "Configuration lookup failed", http.StatusInternalServerError)
		return
	}
	l, err := s.Store.Lock(ctx, c.ID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		s.fail(w, c.ID, "Feed is unavailable", status)
		return
	}
	defer l.Close()
	// A concurrent edit might have changed the endpoint while we waited.
	if l.Config.Path != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	state, err := l.State(ctx)
	if err != nil {
		s.fail(w, c.ID, "Reading feed state failed", http.StatusInternalServerError)
		return
	}
	result, err := s.Source.Get(ctx, l.Config.URL, state.Validators)
	if err != nil {
		var upstream *fetch.Error
		if errors.As(err, &upstream) {
			if upstream.RetryAfter != "" {
				w.Header().Set("Retry-After", upstream.RetryAfter)
			}
			s.recordFailure(l, upstream.Message)
			s.fail(w, c.ID, upstream.Message, upstream.Status)
		} else {
			s.recordFailure(l, "Source request failed")
			s.fail(w, c.ID, "Source request failed", http.StatusBadGateway)
		}
		return
	}
	doc := state.Document
	if !result.NotModified {
		doc, err = feed.Parse(result.Body)
		if err != nil {
			// Errors are local parser/rule diagnostics, never a raw HTTP URL
			// error that could disclose query credentials.
			s.recordFailure(l, err.Error())
			s.fail(w, c.ID, "Source returned an invalid feed; see administration", http.StatusBadGateway)
			return
		}
	} else if doc == nil {
		s.recordFailure(l, "Source returned 304 without a stored document")
		s.fail(w, c.ID, "Source returned 304 without a stored document", http.StatusBadGateway)
		return
	}
	body, err := l.Commit(ctx, doc, result.Validators)
	if err != nil {
		status, detail := http.StatusInternalServerError, "Database operation failed"
		if errors.Is(err, store.ErrInvalidFeed) {
			status, detail = http.StatusBadGateway, safeProcessingError(err)
		}
		s.recordFailure(l, detail)
		s.fail(w, c.ID, "Feed processing failed; see administration", status)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	// Evaluate reader preconditions only AFTER this request contacted the source.
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", stringLength(body))
	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			s.Logger.Debug("reader disconnected", "feed_id", c.ID)
		}
	}
}

func (s *Service) recordFailure(l *store.Locked, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := l.RecordError(ctx, message); err != nil {
		s.Logger.Error("recording feed failure failed", "feed_id", l.Config.ID)
	}
}

func (s *Service) fail(w http.ResponseWriter, id int64, message string, status int) {
	s.Logger.Warn("feed request failed", "feed_id", id, "status", status, "reason", message)
	http.Error(w, message, status)
}
