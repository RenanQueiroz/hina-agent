package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// app bundles the shared dependencies every subcommand needs.
type app struct {
	paths platform.Paths
	cfg   config.Config
	log   *slog.Logger
	logs  *logbuf.Buffer
	store *store.Store
}

// openApp resolves paths, ensures app directories exist, loads config, opens the
// store, and builds the logger. Callers must call close.
func openApp() (*app, error) {
	paths, err := platform.Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	if err := platform.EnsureAll(paths); err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.ConfigFilePath())
	if err != nil {
		return nil, err
	}
	st, err := store.Open(paths.DBPath())
	if err != nil {
		return nil, err
	}
	logs := logbuf.New(500)
	return &app{paths: paths, cfg: cfg, logs: logs, log: newLogger(cfg.Log, logs), store: st}, nil
}

func (a *app) close() {
	if a != nil && a.store != nil {
		_ = a.store.Close()
	}
}

func newLogger(lc config.LogConfig, buf *logbuf.Buffer) *slog.Logger {
	level := slog.LevelInfo
	switch lc.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	w := io.MultiWriter(os.Stderr, buf) // tee logs to the admin ring buffer
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if lc.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}
