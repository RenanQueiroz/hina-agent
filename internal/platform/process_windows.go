//go:build windows

package platform

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platState holds the Job Object the process tree is assigned to.
type platState struct {
	job windows.Handle
}

func (c *Cmd) configureProcAttr() {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A new process group lets us deliver Ctrl-Break independently; the Job
	// Object (assigned in afterStart) is what actually guarantees tree cleanup.
	c.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// afterStart creates a Job Object with KILL_ON_JOB_CLOSE and assigns the running
// process to it, so TerminateJobObject (or process exit) tears down the whole
// tree.
//
// NOTE: there is a small race — descendants spawned between Start and assignment
// are not captured. A CREATE_SUSPENDED + resume variant closes it; that
// refinement and the real-host validation are part of the Windows hardening
// phase. This compiles and is correct for the common case.
func (c *Cmd) afterStart() error {
	if c.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return err
	}
	c.plat.job = job
	return nil
}

func (c *Cmd) killTree() error {
	if c.plat.job == 0 {
		return nil
	}
	err := windows.TerminateJobObject(c.plat.job, 1)
	windows.CloseHandle(c.plat.job)
	c.plat.job = 0
	return err
}
