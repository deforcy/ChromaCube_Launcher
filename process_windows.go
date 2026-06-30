//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process creation flags (values from the Win32 API). We avoid relying
// on x/sys constants for these two so the flags are explicit and stable.
const (
	createNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	createNoWindow        = 0x08000000 // CREATE_NO_WINDOW (no flashing console)
)

// processController on Windows uses a Job Object. Every cloudflared process is
// assigned to its own job that is configured with KILL_ON_JOB_CLOSE, so when we
// close the job handle the OS terminates the process *and any children it may
// have spawned*. This is the reliable way to avoid orphaned processes on
// Windows - Process.Kill() alone would not reap a process tree.
type processController struct {
	job windows.Handle
}

func newProcessController() *processController { return &processController{} }

// prepare runs before Start: put the child in a new process group and suppress
// the console window that would otherwise flash for a console-mode binary.
func (p *processController) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}

// started runs right after Start: create the job object and adopt the process.
func (p *processController) started(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("process not started")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}

	// Configure the job so closing it terminates everything inside.
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
		return fmt.Errorf("configure job object: %w", err)
	}

	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("assign process to job: %w", err)
	}

	p.job = job
	return nil
}

// kill closes the job handle, which (thanks to KILL_ON_JOB_CLOSE) terminates the
// whole tree. Falls back to a direct kill if the job was never set up.
func (p *processController) kill(cmd *exec.Cmd) error {
	if p.job != 0 {
		err := windows.CloseHandle(p.job)
		p.job = 0
		return err
	}
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
