//go:build !windows

package platform

import (
	"os"
	"path/filepath"
)

// dataBase returns the base directory for application data on Unix-like hosts.
// Honors $XDG_DATA_HOME, otherwise ~/.local/share.
//
// NOTE: macOS conventionally prefers ~/Library/Application Support for data; that
// refinement is deferred to the macOS validation work. ~/.local/share is correct
// and harmless on macOS in the meantime.
func dataBase() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return x, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}
