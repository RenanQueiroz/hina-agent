// Package platform is the single OS-abstraction layer for Hina. Everything
// OS-specific — application paths, private-permission enforcement, process-tree
// termination, shutdown signals, and secret-key storage — goes through here
// instead of calling os/os-exec/syscall directly, so the Windows build is
// correct from the first commit. OS-specific logic lives in _unix.go / _windows.go
// files; this file is the cross-platform surface.
package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// appDir is the per-OS application folder name used under every base directory.
const appDir = "hina"

// Paths holds the resolved application directories for this host/user. They are
// rooted at OS-standard locations (never repo-relative — V1's mistake).
type Paths struct {
	Config  string // user-editable config   (os.UserConfigDir/hina)
	Cache   string // model/runtime downloads (os.UserCacheDir/hina)
	Data    string // SQLite DB, vault, workspaces (per-OS data base/hina)
	Runtime string // sockets, per-run scratch, locks (Data/run)
	Log     string // process + setup logs (Data/logs)
}

// Resolve computes the application paths for the current OS/user. It does not
// create them; call EnsureAll for that.
func Resolve() (Paths, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve config dir: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve cache dir: %w", err)
	}
	data, err := dataBase() // per-OS: paths_unix.go / paths_windows.go
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data dir: %w", err)
	}
	p := Paths{
		Config: filepath.Join(cfg, appDir),
		Cache:  filepath.Join(cache, appDir),
		Data:   filepath.Join(data, appDir),
	}
	p.Runtime = filepath.Join(p.Data, "run")
	p.Log = filepath.Join(p.Data, "logs")
	return p, nil
}

// EnsureAll creates every application directory with private permissions.
func EnsureAll(p Paths) error {
	for _, d := range []string{p.Config, p.Cache, p.Data, p.Runtime, p.Log} {
		if err := EnsurePrivateDir(d); err != nil {
			return err
		}
	}
	return nil
}

// DBPath is the canonical SQLite database location.
func (p Paths) DBPath() string { return filepath.Join(p.Data, "hina.db") }

// ConfigFilePath is the canonical config file location.
func (p Paths) ConfigFilePath() string { return filepath.Join(p.Config, "config.toml") }

// MasterKeyPath is where the local secret-vault master key is stored.
func (p Paths) MasterKeyPath() string { return filepath.Join(p.Data, "master.key") }
