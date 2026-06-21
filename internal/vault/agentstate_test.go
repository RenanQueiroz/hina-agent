package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func TestAgentStateRoundTrip(t *testing.T) {
	v, _, uid, _, root := newTestVault(t)

	if v.HasAgentState(uid, "codex") {
		t.Fatal("HasAgentState should be false before Put")
	}
	if _, err := v.GetAgentState(uid, "codex"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get before Put = %v, want ErrNotFound", err)
	}

	blob := []byte("tar-archive-of-CODEX_HOME\x00\x01binary")
	if err := v.PutAgentState(uid, "codex", blob); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !v.HasAgentState(uid, "codex") {
		t.Fatal("HasAgentState should be true after Put")
	}
	got, err := v.GetAgentState(uid, "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	// The on-disk blob must NOT contain the plaintext (envelope-encrypted).
	path := filepath.Join(root, uid, "agents", "codex.enc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if bytes.Contains(raw, []byte("tar-archive-of-CODEX_HOME")) {
		t.Fatal("plaintext leaked into the on-disk agent-state blob")
	}

	// Replace, then delete.
	if err := v.PutAgentState(uid, "codex", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.GetAgentState(uid, "codex"); string(got) != "new" {
		t.Fatalf("replace failed: %q", got)
	}
	if err := v.DeleteAgentState(uid, "codex"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if v.HasAgentState(uid, "codex") {
		t.Fatal("HasAgentState should be false after Delete")
	}
	// Deleting an absent blob is tolerated.
	if err := v.DeleteAgentState(uid, "codex"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

func TestAgentStateRejectsUnsafeComponents(t *testing.T) {
	v, _, _, _, _ := newTestVault(t)
	if err := v.PutAgentState("../escape", "codex", []byte("x")); err == nil {
		t.Fatal("PutAgentState should reject an unsafe user id")
	}
	if err := v.PutAgentState("user", "../escape", []byte("x")); err == nil {
		t.Fatal("PutAgentState should reject an unsafe provider")
	}
}

func TestAgentStateIsolatedPerProviderAndUser(t *testing.T) {
	v, _, uid, _, _ := newTestVault(t)
	if err := v.PutAgentState(uid, "codex", []byte("codex-state")); err != nil {
		t.Fatal(err)
	}
	if err := v.PutAgentState(uid, "claude", []byte("claude-state")); err != nil {
		t.Fatal(err)
	}
	c, _ := v.GetAgentState(uid, "codex")
	cl, _ := v.GetAgentState(uid, "claude")
	if string(c) != "codex-state" || string(cl) != "claude-state" {
		t.Fatalf("provider blobs crossed: %q / %q", c, cl)
	}
}
