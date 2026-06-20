//go:build !windows

package assets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An asset that is a SYMLINK (e.g. into attacker-writable storage) must not verify
// or be read, even if its target currently has the right size/bytes — the link's
// target could be swapped after the checksum.
func TestVerifyRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	a, _ := ORTAsset("linux", "amd64")
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), int(a.MemberSize)), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, a.Dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	st := verifyAsset(root, a)
	if st.Verified {
		t.Fatal("a symlinked asset must not verify")
	}
	if !strings.Contains(st.Reason, "symlink") {
		t.Fatalf("reason = %q, want a symlink-rejection reason", st.Reason)
	}
}

// A symlinked INTERMEDIATE path component (e.g. root/ort -> attacker dir) must also
// be rejected, even when the final file is a regular file with the right bytes.
func TestVerifyRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	a, _ := ORTAsset("linux", "amd64")
	// Build the real asset at an out-of-root location, then symlink root/ort to it.
	outside := filepath.Join(t.TempDir(), "evil-ort")
	libRel := strings.TrimPrefix(a.Dest, "ort/") // lib/<name>
	libPath := filepath.Join(outside, libRel)
	if err := os.MkdirAll(filepath.Dir(libPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libPath, bytes.Repeat([]byte("x"), int(a.MemberSize)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ort")); err != nil {
		t.Fatal(err)
	}
	st := verifyAsset(root, a)
	if st.Verified {
		t.Fatal("an asset reached through a symlinked parent must not verify")
	}
	if !strings.Contains(st.Reason, "symlink") {
		t.Fatalf("reason = %q, want a symlink-rejection reason", st.Reason)
	}
}

func TestReadVerifiedRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	var m1 supModel
	for _, m := range supModels {
		if m.path == "voice_styles/M1.json" {
			m1 = m
		}
	}
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), int(m1.size)), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, m1.dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerified(root, m1.dest); err == nil {
		t.Fatal("ReadVerified must reject a symlinked asset")
	}
}
