package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestWorkspaces(t *testing.T) *WorkspaceManager {
	t.Helper()
	w, err := NewWorkspaceManager(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "run"), nil)
	if err != nil {
		t.Fatalf("new workspaces: %v", err)
	}
	return w
}

func TestUserAndSessionWorkspace(t *testing.T) {
	w := newTestWorkspaces(t)
	uid := "usr_abc"
	ws, err := w.UserWorkspace(uid)
	if err != nil {
		t.Fatalf("user workspace: %v", err)
	}
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		t.Fatalf("user workspace not created: %v", err)
	}
	// Idempotent.
	if ws2, _ := w.UserWorkspace(uid); ws2 != ws {
		t.Fatalf("user workspace not stable: %q vs %q", ws, ws2)
	}
	sess, err := w.SessionWorkspace(uid, "cnv_1")
	if err != nil {
		t.Fatalf("session workspace: %v", err)
	}
	if st, err := os.Stat(sess); err != nil || !st.IsDir() {
		t.Fatalf("session workspace not created: %v", err)
	}
}

func TestWorkspaceDurableAcrossManagers(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	runDir := filepath.Join(t.TempDir(), "run")
	w1, err := NewWorkspaceManager(dataDir, runDir, nil)
	if err != nil {
		t.Fatalf("w1: %v", err)
	}
	ws, _ := w1.UserWorkspace("usr_x")
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A fresh manager (simulating a server restart) sees the same durable file.
	w2, err := NewWorkspaceManager(dataDir, runDir, nil)
	if err != nil {
		t.Fatalf("w2: %v", err)
	}
	ws2, _ := w2.UserWorkspace("usr_x")
	if b, err := os.ReadFile(filepath.Join(ws2, "keep.txt")); err != nil || string(b) != "data" {
		t.Fatalf("durable workspace lost across restart: %q err=%v", b, err)
	}
}

func TestScratchLifecycle(t *testing.T) {
	w := newTestWorkspaces(t)
	s, err := w.NewScratch()
	if err != nil {
		t.Fatalf("scratch: %v", err)
	}
	if st, err := os.Stat(s.Dir); err != nil || !st.IsDir() {
		t.Fatalf("scratch not created: %v", err)
	}
	s.Remove()
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatalf("scratch not removed: %v", err)
	}
}

func TestCleanScratchReapsStale(t *testing.T) {
	w := newTestWorkspaces(t)
	stale, _ := w.NewScratch()
	fresh, _ := w.NewScratch()
	// Age the stale scratch past the TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale.Dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	n, err := w.CleanScratch(time.Hour)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if _, err := os.Stat(stale.Dir); !os.IsNotExist(err) {
		t.Fatal("stale scratch should be gone")
	}
	if _, err := os.Stat(fresh.Dir); err != nil {
		t.Fatal("fresh scratch should survive")
	}
}

func TestUsageAndQuota(t *testing.T) {
	w := newTestWorkspaces(t)
	ws, _ := w.UserWorkspace("usr_q")
	if err := os.WriteFile(filepath.Join(ws, "f"), make([]byte, 1024), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	used, err := w.UserUsageBytes("usr_q")
	if err != nil || used < 1024 {
		t.Fatalf("usage = %d err=%v", used, err)
	}
	if ok, _, _ := w.WithinQuota("usr_q", 512); ok {
		t.Fatal("should be over a 512-byte quota")
	}
	if ok, _, _ := w.WithinQuota("usr_q", 0); !ok {
		t.Fatal("quota 0 means unlimited")
	}
}

func TestWorkspaceRejectsUnsafeID(t *testing.T) {
	w := newTestWorkspaces(t)
	if _, err := w.UserWorkspace("../escape"); err == nil {
		t.Fatal("unsafe user id should be rejected")
	}
	if _, err := w.SessionWorkspace("usr_ok", "../escape"); err == nil {
		t.Fatal("unsafe session id should be rejected")
	}
}
