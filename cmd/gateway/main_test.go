package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestRunRejectsMissingConfig exercises run's early failure path: a missing config file must
// return an error before any listener is opened, so the test never binds a port.
func TestRunRejectsMissingConfig(t *testing.T) {
	if err := run(filepath.Join(t.TempDir(), "does-not-exist.yaml"), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("run with a missing config should return an error")
	}
}

// TestRunRejectsInvalidConfig: a syntactically valid file that fails validation (no
// master_key, no providers) must also fail fast.
func TestRunRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":0\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := run(path, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("run with an invalid config should return an error")
	}
}
