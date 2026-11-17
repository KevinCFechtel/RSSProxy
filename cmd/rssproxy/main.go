package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/KevinCFechtel/RSSProxy/internal/app"
	"github.com/KevinCFechtel/RSSProxy/internal/store"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			return healthcheck()
		case "version":
			fmt.Println(version)
			return nil
		default:
			return errors.New("usage: rssproxy [healthcheck|version]")
		}
	}
	c, err := app.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	db, err := store.Open(startup, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(startup); err != nil {
		return errors.New("database migration failed; verify database permissions and schema")
	}
	imported, err := db.ImportLegacy(startup, c.LegacyDatabaseURL)
	if err != nil {
		return fmt.Errorf("legacy import failed: %w", err)
	}
	logger.Info("database initialized", "imported_feeds", imported)
	handler, err := app.Handler(c, db, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: c.ListenAddr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: c.RequestTimeout + 10*time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10,
		// The server's default logger includes request URLs in some diagnostics.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	listener, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		return errors.New("HTTP listen failed; check LISTEN_ADDR")
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("RSSProxy started", "version", version, "listen", listener.Addr().String())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), c.RequestTimeout+5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("graceful shutdown deadline exceeded")
		}
		return nil
	}
}

func healthcheck() error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("invalid healthcheck address")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(strings.Trim(host, "[]"), port) + "/readyz")
	if err != nil {
		return errors.New("healthcheck connection failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode != http.StatusOK {
		return errors.New("healthcheck failed")
	}
	return nil
}
