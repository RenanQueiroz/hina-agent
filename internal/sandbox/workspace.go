package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// safeComponent guards path components built from ids (defense in depth — ids are
// server-issued and always safe).
var safeComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// WorkspaceManager owns the on-disk storage sandboxes use: a durable per-user
// workspace (and optional per-session sub-workspace) under the data dir that
// survives restarts and container teardown, and ephemeral per-run scratch under
// the runtime dir that a janitor reaps. Root filesystems are recreated from the
// `sbx` kit, not mutated, so only this app-managed storage needs lifecycle care.
type WorkspaceManager struct {
	base    string // <dataDir>/workspaces  (durable)
	scratch string // <runtimeDir>/scratch  (ephemeral)
	log     *slog.Logger
}

// NewWorkspaceManager creates the durable + ephemeral roots as owner-private
// directories.
func NewWorkspaceManager(dataDir, runtimeDir string, log *slog.Logger) (*WorkspaceManager, error) {
	if log == nil {
		log = slog.Default()
	}
	w := &WorkspaceManager{
		base:    filepath.Join(dataDir, "workspaces"),
		scratch: filepath.Join(runtimeDir, "scratch"),
		log:     log,
	}
	if err := platform.EnsurePrivateDir(w.base); err != nil {
		return nil, fmt.Errorf("sandbox: workspace root: %w", err)
	}
	if err := platform.EnsurePrivateDir(w.scratch); err != nil {
		return nil, fmt.Errorf("sandbox: scratch root: %w", err)
	}
	return w, nil
}

// UserWorkspace returns (creating if needed) the durable workspace for a user.
func (w *WorkspaceManager) UserWorkspace(userID string) (string, error) {
	if !safeComponent.MatchString(userID) {
		return "", fmt.Errorf("sandbox: unsafe user id")
	}
	dir := filepath.Join(w.base, userID)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("sandbox: user workspace: %w", err)
	}
	return dir, nil
}

// SessionWorkspace returns (creating if needed) a durable per-session workspace
// nested under the user's workspace.
func (w *WorkspaceManager) SessionWorkspace(userID, sessionID string) (string, error) {
	if !safeComponent.MatchString(sessionID) {
		return "", fmt.Errorf("sandbox: unsafe session id")
	}
	root, err := w.UserWorkspace(userID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "sessions", sessionID)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("sandbox: session workspace: %w", err)
	}
	return dir, nil
}

// Scratch is one ephemeral run-scratch directory and its cleanup. The cleanup is
// idempotent; the janitor also reaps any scratch a crashed run left behind.
type Scratch struct {
	Dir     string
	cleanup func()
}

// Remove deletes the scratch directory.
func (s Scratch) Remove() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

// NewScratch creates a fresh ephemeral scratch directory for one run.
func (w *WorkspaceManager) NewScratch() (Scratch, error) {
	dir := filepath.Join(w.scratch, id.New("run"))
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return Scratch{}, fmt.Errorf("sandbox: scratch: %w", err)
	}
	return Scratch{Dir: dir, cleanup: func() {
		if err := os.RemoveAll(dir); err != nil {
			w.log.Warn("sandbox: scratch cleanup failed", "dir", dir, "err", err)
		}
	}}, nil
}

// UserUsageBytes returns the total bytes used by a user's durable workspace, for
// quota enforcement and admin usage visibility.
func (w *WorkspaceManager) UserUsageBytes(userID string) (int64, error) {
	dir, err := w.UserWorkspace(userID)
	if err != nil {
		return 0, err
	}
	return dirSize(dir)
}

// WithinQuota reports whether a user's durable workspace is at or under quotaBytes.
// quotaBytes <= 0 means unlimited.
func (w *WorkspaceManager) WithinQuota(userID string, quotaBytes int64) (bool, int64, error) {
	used, err := w.UserUsageBytes(userID)
	if err != nil {
		return false, 0, err
	}
	if quotaBytes <= 0 {
		return true, used, nil
	}
	return used <= quotaBytes, used, nil
}

// CleanScratch removes ephemeral scratch directories whose mtime is older than
// maxAge, returning how many were reaped. This is the janitor's core step; it is
// safe to call concurrently with active runs (those dirs are freshly mtime'd).
func (w *WorkspaceManager) CleanScratch(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(w.scratch)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(w.scratch, e.Name())
			if err := os.RemoveAll(path); err != nil {
				w.log.Warn("sandbox: janitor failed to remove scratch", "dir", path, "err", err)
				continue
			}
			reaped++
		}
	}
	return reaped, nil
}

// Janitor runs CleanScratch on a ticker until ctx is cancelled. The server starts
// it in a goroutine so stale scratch from crashed runs can't accumulate.
func (w *WorkspaceManager) Janitor(ctx context.Context, interval, scratchTTL time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := w.CleanScratch(scratchTTL); err != nil {
				w.log.Warn("sandbox: janitor error", "err", err)
			} else if n > 0 {
				w.log.Info("sandbox: janitor reaped scratch", "count", n)
			}
		}
	}
}

// dirSize sums the sizes of all regular files under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}
