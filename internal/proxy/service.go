package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	var doc *feed.Document
	var validators fetch.Validators
	if l.Config.Type == "group" {
		doc, err = s.fetchGroup(ctx, l)
	} else {
		doc, validators, err = s.fetchDocument(ctx, l)
	}
	if err != nil {
		var upstream *fetch.Error
		switch {
		case errors.As(err, &upstream):
			if upstream.RetryAfter != "" {
				w.Header().Set("Retry-After", upstream.RetryAfter)
			}
			s.recordFailure(l, upstream.Message)
			s.fail(w, c.ID, upstream.Message, upstream.Status)
		case errors.Is(err, store.ErrInvalidFeed):
			s.recordFailure(l, safeProcessingError(err))
			s.fail(w, c.ID, "Source returned an invalid feed; see administration", http.StatusBadGateway)
		default:
			s.recordFailure(l, "Database operation failed")
			s.fail(w, c.ID, "Database operation failed", http.StatusInternalServerError)
		}
		return
	}
	body, err := l.Commit(ctx, doc, validators)
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

// Each member lock is released before the next is acquired. Nested groups are
// forbidden, so overlapping groups cannot create lock cycles.
func (s *Service) fetchGroup(ctx context.Context, group *store.Locked) (*feed.Document, error) {
	sources := make([]feed.GroupSource, 0, len(group.Config.SourceIDs))
	for _, id := range group.Config.SourceIDs {
		source, err := group.LockSource(ctx, id)
		if err != nil {
			return nil, err
		}
		doc, sourceErr := s.fetchGroupMember(ctx, source)
		sourceURL := source.Config.URL
		source.Close()
		if sourceErr != nil {
			return nil, sourceErr
		}
		sources = append(sources, feed.GroupSource{URL: sourceURL, Document: doc})
	}
	doc, err := feed.MergeGroup(group.Config, sources)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalidFeed, err)
	}
	return doc, nil
}

func (s *Service) fetchGroupMember(ctx context.Context, member *store.Locked) (*feed.Document, error) {
	if member.Config.Type != "" {
		return nil, store.ErrInvalidGroup
	}
	doc, validators, err := s.fetchDocument(ctx, member)
	if err != nil {
		message := "Database operation failed"
		var upstream *fetch.Error
		if errors.As(err, &upstream) {
			message = upstream.Message
		} else if errors.Is(err, store.ErrInvalidFeed) {
			message = safeProcessingError(err)
		}
		s.recordFailure(member, message)
		return nil, err
	}
	if err := member.StoreDocument(ctx, doc, validators); err != nil {
		return nil, err
	}
	return doc, nil
}

func (s *Service) fetchDocument(ctx context.Context, l *store.Locked) (*feed.Document, fetch.Validators, error) {
	state, err := l.State(ctx)
	if err != nil {
		return nil, fetch.Validators{}, err
	}
	result, err := s.Source.Get(ctx, l.Config.URL, state.Validators)
	if err != nil {
		var upstream *fetch.Error
		if !errors.As(err, &upstream) {
			err = &fetch.Error{Message: "Source request failed", Status: http.StatusBadGateway}
		}
		return nil, fetch.Validators{}, err
	}
	doc := state.Document
	if !result.NotModified {
		doc, err = feed.Parse(result.Body)
		if err != nil {
			return nil, fetch.Validators{}, fmt.Errorf("%w: %w", store.ErrInvalidFeed, err)
		}
	} else if doc == nil {
		return nil, fetch.Validators{}, fmt.Errorf("%w: source returned 304 without a stored document", store.ErrInvalidFeed)
	}
	return doc, result.Validators, nil
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
