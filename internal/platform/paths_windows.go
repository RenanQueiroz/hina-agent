//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// dataBase returns the base directory for application data on Windows
// (%LocalAppData%, falling back to the user home).
func dataBase() (string, error) {
	if l := os.Getenv("LocalAppData"); l != "" {
		return l, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "AppData", "Local"), nil
}
