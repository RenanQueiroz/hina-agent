//go:build !windows && !darwin

package platform

import (
	"os"
	"path/filepath"
)

// dataBase returns the base directory for application data on Linux/BSD hosts.
// Honors $XDG_DATA_HOME, otherwise ~/.local/share. macOS has its own
// implementation (paths_darwin.go) that uses ~/Library/Application Support.
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
