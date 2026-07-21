//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchAgentLabel is both the LaunchAgent's Label and its plist file name.
const launchAgentLabel = "com.chromacube.launcher"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// applyAutostart installs or removes a per-user LaunchAgent that starts the app
// at login (the macOS analogue of the Windows logon scheduled task).
//
// The agent launches the app UNPRIVILEGED. That is deliberate: a login-time
// LaunchAgent cannot elevate, and running the whole UI as root would be wrong.
// When hostname mode later needs to edit /etc/hosts, the app requests admin
// rights just for that write (see commitHostsFile). --tray asks the app to
// start without stealing focus at login.
func applyAutostart(enable bool) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}

	if !enable {
		_ = exec.Command("launchctl", "unload", path).Run()
		_ = os.Remove(path)
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>--tray</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	// Reload so the change takes effect without requiring a re-login. Errors are
	// non-fatal: the plist on disk is what matters at the next login.
	_ = exec.Command("launchctl", "unload", path).Run()
	_ = exec.Command("launchctl", "load", path).Run()
	return nil
}

// xmlEscape escapes the characters that would be invalid inside the plist's
// <string> element (the executable path can in principle contain '&').
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
