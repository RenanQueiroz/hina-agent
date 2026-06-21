//go:build unix

package agentauth

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// TestCLISessionFinalizesOnCleanExit proves the pipe lifecycle: a process that exits
// cleanly still drives the reader to EOF (so the broker's read-loop-then-Wait pattern
// finalizes the login) — the wait goroutine in newCLISession closes the pipe writer
// on exit. Without that, a successful login would deadlock here.
func TestCLISessionFinalizesOnCleanExit(t *testing.T) {
	cmd := platform.Command(context.Background(), "/bin/sh", "-c", "printf 'Logged in\\n'")
	sess, err := newCLISession(context.Background(), cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(sess.Stdout()) // blocks until EOF
		out <- string(b)
	}()
	select {
	case got := <-out:
		if !strings.Contains(got, "Logged in") {
			t.Errorf("output = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read never reached EOF on a clean exit — a real login would deadlock")
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// TestCLISessionTimeoutReapsTree proves a login that overruns its context deadline
// has its whole process tree reaped: a script spawns a background child, then hangs.
// On the timeout the session must KillTree the group (not just the immediate process,
// which would leave the network-on container/children + credential-store mount alive).
func TestCLISessionTimeoutReapsTree(t *testing.T) {
	dir := t.TempDir()
	childPid := filepath.Join(dir, "childpid")
	script := filepath.Join(dir, "login.sh")
	content := "#!/bin/sh\nsleep 30 &\necho $! > " + childPid + "\necho started\nsleep 30\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sess, err := newCLISession(ctx, platform.Command(ctx, script))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _, _ = io.ReadAll(sess.Stdout()) }() // drain so the pipe doesn't block the child

	done := make(chan struct{})
	go func() { _ = sess.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the login timeout")
	}

	b, _ := os.ReadFile(childPid)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("no child pid recorded: %q", b)
	}
	dead := false
	for i := 0; i < 200; i++ {
		if syscall.Kill(pid, 0) != nil {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("login child %d survived the timeout (tree not reaped)", pid)
	}
}

// TestCLISessionWriteAndKill exercises the input + kill paths against a real process.
func TestCLISessionWriteAndKill(t *testing.T) {
	cmd := platform.Command(context.Background(), "/bin/sh", "-c", "cat")
	sess, err := newCLISession(context.Background(), cmd)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := sess.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sess.Kill()
	done := make(chan struct{})
	go func() { _ = sess.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}
