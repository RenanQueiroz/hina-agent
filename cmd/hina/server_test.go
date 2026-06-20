//go:build !windows

package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// secureAssetRoot must lock a group/world-writable asset root down to owner-only
// (0700) before it is trusted for native-library loading, so another local
// principal can't swap an asset in the verify->dlopen window.
func TestSecureAssetRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil { // start group/world-writable
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if !secureAssetRoot(dir, log) {
		t.Fatal("secureAssetRoot should succeed for an owned directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("asset root mode = %#o after securing, want 0700 (no group/other access)", perm)
	}
}
