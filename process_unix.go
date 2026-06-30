//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// processController on Unix (macOS/Linux) relies on process groups. By starting
// cloudflared in its own group (Setpgid) we can signal the entire group with a
// single kill to the negative PID, which reaps the process and any descendants -
// the reliable way to avoid orphaned processes here.
type processController struct{}

func newProcessController() *processController { return &processController{} }

// prepare puts the child into a new session/process group before Start.
func (p *processController) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// started is a no-op on Unix; the group was configured in prepare().
func (p *processController) started(cmd *exec.Cmd) error { return nil }

// kill sends SIGTERM to the whole process group for a graceful shutdown, then
// SIGKILL shortly after as a guarantee. Signalling -pid targets the group.
func (p *processController) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid

	// Polite request first.
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	// Give it a brief moment to exit cleanly, then force-kill the group.
	go func(pgid int) {
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}(pgid)

	return nil
}
