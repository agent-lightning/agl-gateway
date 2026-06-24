// Command gateway runs the agl-gateway LLM proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/admin"
	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/portal"
	"github.com/agent-lightning/agl-gateway/internal/pricing"
	"github.com/agent-lightning/agl-gateway/internal/proxy"
	"github.com/agent-lightning/agl-gateway/internal/server"
	"github.com/agent-lightning/agl-gateway/internal/store"
	"github.com/agent-lightning/agl-gateway/internal/version"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	showVersion := flag.Bool("version", false, "print the gateway version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	st, err := store.OpenWithLogs(cfg.Database, cfg.LogsDatabase)
	if err != nil {
		return err
	}
	defer st.Close()

	prices := pricing.New(cfg.Pricing)
	httpClient := &http.Client{Transport: http.DefaultTransport}

	p := proxy.New(cfg, st, prices, httpClient, logger)
	a := admin.New(cfg, st, httpClient, p, logger)
	handler := server.New(p, a, portal.Handler())

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("agl-gateway listening",
			"version", version.Version,
			"addr", cfg.Server.Addr, "providers", len(cfg.Providers), "database", redactDatabase(cfg.Database),
			"logs_database", redactDatabase(cfg.LogsDatabase))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// redactDatabase masks any password in a postgres:// or clickhouse:// DSN so it is safe to
// log. A SQLite file path (no userinfo) and an empty string are returned unchanged.
func redactDatabase(database string) string {
	if strings.HasPrefix(database, "postgres://") || strings.HasPrefix(database, "postgresql://") ||
		strings.HasPrefix(database, "clickhouse://") || strings.HasPrefix(database, "clickhouses://") {
		if u, err := url.Parse(database); err == nil {
			return u.Redacted()
		}
		return "[redacted]"
	}
	return database
}
