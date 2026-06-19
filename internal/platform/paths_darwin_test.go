//go:build darwin

package platform

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDarwinDataBaseIsApplicationSupport pins the Phase 1 macOS path contract:
// application data lives under ~/Library/Application Support, not ~/.local/share.
func TestDarwinDataBaseIsApplicationSupport(t *testing.T) {
	base, err := dataBase()
	if err != nil {
		t.Fatalf("dataBase: %v", err)
	}
	want := filepath.Join("Library", "Application Support")
	if !strings.Contains(base, want) {
		t.Fatalf("macOS data base = %q, want a path under %q", base, want)
	}
}
