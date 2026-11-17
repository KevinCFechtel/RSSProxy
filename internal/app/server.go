package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/fetch"
	"github.com/KevinCFechtel/RSSProxy/internal/proxy"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
	"github.com/KevinCFechtel/RSSProxy/internal/web"
)

func Handler(c Config, db *store.Store, logger *slog.Logger) (http.Handler, error) {
	admin, err := web.NewAdmin(db, logger, c.AuthUser, c.AuthPassword, c.CookieSecure)
	if err != nil {
		return nil, err
	}
	service := &proxy.Service{
		Store: db, Source: fetch.New(c.SourceTimeout, c.MaxFeedBytes, c.AllowedNetworks),
		Logger: logger, Timeout: c.RequestTimeout,
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin.Handler())
	mux.Handle("/admin", http.RedirectHandler("/admin/", http.StatusSeeOther))
	mux.Handle("GET /configs", http.RedirectHandler("/admin/", http.StatusSeeOther))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		if err := db.Pool.Ping(ctx); err != nil {
			http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.Handle("/", service)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				logger.Error("unhandled HTTP panic")
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		mux.ServeHTTP(w, r)
	}), nil
}
