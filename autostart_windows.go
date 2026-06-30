//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// autostartTask is the Task Scheduler task name.
const autostartTask = "ChromaCubeLauncher"

// applyAutostart adds or removes a logon scheduled task that launches the app.
//
// We use Task Scheduler (not the HKCU Run key) because this app requires
// administrator (for editing the hosts file): the Run key cannot silently
// elevate at logon, but a task created with "highest privileges" (/RL HIGHEST)
// launches the elevated app at logon with no UAC prompt. Creating such a task
// itself needs elevation, which we already have.
func applyAutostart(enable bool) error {
	if enable {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return runHidden("schtasks", "/Create",
			"/TN", autostartTask,
			"/TR", exe, // exec quotes this; handles spaces in the path
			"/SC", "ONLOGON",
			"/RL", "HIGHEST",
			"/F",
		)
	}
	// Deleting a non-existent task is not fatal.
	_ = runHidden("schtasks", "/Delete", "/TN", autostartTask, "/F")
	return nil
}

// runHidden runs a console command without flashing a console window.
func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Run()
}
