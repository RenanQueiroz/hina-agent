//go:build !windows

package platform

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillTreeLeavesNoOrphans is the Phase 1 exit criterion for process
// supervision: a child that spawns a long-sleeping grandchild must leave no
// orphan after KillTree.
func TestKillTreeLeavesNoOrphans(t *testing.T) {
	ctx := context.Background()
	// The child shell backgrounds a 60s sleep (the grandchild), prints its pid,
	// then waits so the whole tree stays alive until we kill it.
	c := Command(ctx, "sh", "-c", "sleep 60 & echo $!; wait")
	stdout, err := c.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	gpid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", line, err)
	}

	if !processAlive(gpid) {
		t.Fatalf("grandchild %d not alive before kill", gpid)
	}
	if err := c.KillTree(); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	_ = c.Wait()

	// Poll briefly for the grandchild to disappear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(gpid) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still alive after KillTree", gpid)
}

// processAlive reports whether pid refers to a live process (signal 0 probe).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != syscall.ESRCH
}
