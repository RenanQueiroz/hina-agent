//go:build windows

package platform

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestKillTreeLeavesNoOrphans is the Windows side of the Phase 1 process-tree
// supervision exit criterion (mirrors the Unix test): a child that spawns a
// long-sleeping grandchild must leave no orphan after KillTree, proving the Job
// Object tears down the whole tree. Runs on the windows-latest CI runner via
// `go test ./...`.
func TestKillTreeLeavesNoOrphans(t *testing.T) {
	ctx := context.Background()
	// The child PowerShell starts a 60s-sleeping grandchild PowerShell, prints
	// its PID, then sleeps so the tree stays alive until KillTree.
	const script = `$p = Start-Process powershell -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 60' -PassThru; Write-Output $p.Id; Start-Sleep -Seconds 60`
	c := Command(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(gpid) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d still alive after KillTree", gpid)
}

// processAlive reports whether pid refers to a live process. A 0-timeout wait on
// the process handle returns WAIT_TIMEOUT while it runs and WAIT_OBJECT_0 once it
// has exited; an unopenable PID is treated as gone.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	s, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return s == uint32(windows.WAIT_TIMEOUT)
}
