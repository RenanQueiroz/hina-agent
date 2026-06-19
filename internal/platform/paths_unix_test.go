//go:build !windows && !darwin

package platform

import (
	"path/filepath"
	"testing"
)

// TestUnixDataBaseHonorsXDG pins the Linux/BSD data-path contract: honor
// $XDG_DATA_HOME, else fall back to ~/.local/share.
func TestUnixDataBaseHonorsXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-data-home")
	base, err := dataBase()
	if err != nil {
		t.Fatalf("dataBase: %v", err)
	}
	if base != "/tmp/xdg-data-home" {
		t.Fatalf("with XDG_DATA_HOME set, data base = %q, want /tmp/xdg-data-home", base)
	}

	t.Setenv("XDG_DATA_HOME", "")
	base, err = dataBase()
	if err != nil {
		t.Fatalf("dataBase: %v", err)
	}
	if filepath.Base(filepath.Dir(base)) != ".local" || filepath.Base(base) != "share" {
		t.Fatalf("fallback data base = %q, want a ~/.local/share path", base)
	}
}
