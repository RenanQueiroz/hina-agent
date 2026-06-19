package platform

import (
	"context"
	"os/exec"
)

// Cmd wraps exec.Cmd with reliable process-tree termination. exec.CommandContext
// alone does not kill grandchildren spawned by model servers / setup helpers, so
// KillTree uses a new process group (Unix) or a Job Object (Windows). The
// per-OS state and behavior live in process_unix.go / process_windows.go.
type Cmd struct {
	*exec.Cmd
	plat platState
}

// Command builds a process-tree-aware command. Argv-first (no shell) by design.
func Command(ctx context.Context, name string, args ...string) *Cmd {
	c := &Cmd{Cmd: exec.CommandContext(ctx, name, args...)}
	c.configureProcAttr()
	return c
}

// Start launches the process and registers it for tree cleanup.
func (c *Cmd) Start() error {
	if err := c.Cmd.Start(); err != nil {
		return err
	}
	return c.afterStart()
}

// Run starts the process and waits for it to exit.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// KillTree terminates the process and all of its descendants. Safe to call even
// if the process already exited.
func (c *Cmd) KillTree() error { return c.killTree() }
