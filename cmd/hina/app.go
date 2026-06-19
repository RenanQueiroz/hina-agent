package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// app bundles the shared dependencies every subcommand needs.
type app struct {
	paths platform.Paths
	cfg   config.Config
	log   *slog.Logger
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
	return &app{paths: paths, cfg: cfg, log: newLogger(cfg.Log), store: st}, nil
}

func (a *app) close() {
	if a != nil && a.store != nil {
		_ = a.store.Close()
	}
}

func newLogger(lc config.LogConfig) *slog.Logger {
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
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if lc.Format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
