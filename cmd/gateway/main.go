// Command gateway runs the agl-gateway LLM proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kiki/agl-gateway/internal/admin"
	"github.com/kiki/agl-gateway/internal/config"
	"github.com/kiki/agl-gateway/internal/portal"
	"github.com/kiki/agl-gateway/internal/pricing"
	"github.com/kiki/agl-gateway/internal/proxy"
	"github.com/kiki/agl-gateway/internal/server"
	"github.com/kiki/agl-gateway/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

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

	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	prices := pricing.New(cfg.Pricing)
	httpClient := &http.Client{Transport: http.DefaultTransport}

	p := proxy.New(cfg, st, prices, httpClient, logger)
	a := admin.New(cfg, st, logger)
	handler := server.New(p, a, portal.Handler())

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("agl-gateway listening",
			"addr", cfg.Server.Addr, "providers", len(cfg.Providers), "database", cfg.Database)
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
