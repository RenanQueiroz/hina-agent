//go:build unix

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunTimeoutReapsDetachedChild proves the per-run timeout reaps the whole
// process tree: a shim spawns a background child (in the same process group) that
// holds the output pipe, then hangs. On the deadline the runner must KillTree the
// group — killing the child and unblocking Wait — and mark the run timed out.
func TestRunTimeoutReapsDetachedChild(t *testing.T) {
	dir := t.TempDir()
	childPid := filepath.Join(dir, "childpid")
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'sbx version 0.33.0'; exit 0; fi\n" +
		"sleep 30 &\n" + // detached child, same process group, inherits the output pipe
		"echo $! > " + childPid + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewCLIRunner(Config{Path: path, OutputDir: t.TempDir()})
	res, err := r.Run(context.Background(), RunSpec{
		Tool:   ToolShell,
		Argv:   []string{"sleep"},
		Limits: Limits{Timeout: 300 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected the run to be marked timed out")
	}

	b, _ := os.ReadFile(childPid)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("no child pid recorded: %q", b)
	}
	// The background child must be reaped (KillTree on the deadline). Poll a moment
	// for the kernel to tear it down.
	dead := false
	for i := 0; i < 200; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("detached child %d survived the timeout (not reaped)", pid)
	}
}

// TestVersionProbeReapsDetachedChild proves the version probe reaps a background
// child even on a NORMAL (successful) exit — the child closes stdio so it doesn't
// hold the pipe and Wait returns promptly; KillTree must still reap it.
func TestVersionProbeReapsDetachedChild(t *testing.T) {
	dir := t.TempDir()
	childPid := filepath.Join(dir, "childpid")
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then\n" +
		"  sleep 30 </dev/null >/dev/null 2>&1 &\n" + // closes stdio -> doesn't hold the pipe
		"  echo $! > " + childPid + "\n" +
		"  echo 'sbx version 0.33.0'\n" +
		"  exit 0\n" +
		"fi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewCLIRunner(Config{Path: path})
	if !r.Available() {
		t.Fatalf("runner should be available: %s", r.Status().Reason)
	}
	b, _ := os.ReadFile(childPid)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("no child pid recorded: %q", b)
	}
	dead := false
	for i := 0; i < 200; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("probe child %d survived a normal exit (not reaped)", pid)
	}
}
