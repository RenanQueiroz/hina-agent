//go:build !windows

package platform

import "syscall"

// platState carries no extra state on Unix — the process group is keyed by pid.
type platState struct{}

func (c *Cmd) configureProcAttr() {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Put the child in its own process group so the whole tree shares a pgid.
	c.SysProcAttr.Setpgid = true
}

func (c *Cmd) afterStart() error { return nil }

func (c *Cmd) killTree() error {
	if c.Process == nil {
		return nil
	}
	// A negative pid signals the entire process group (child + descendants).
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil // already gone
		}
		return err
	}
	return nil
}
