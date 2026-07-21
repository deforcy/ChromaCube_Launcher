//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// notify shows a native macOS notification via osascript. Best-effort: the
// error is returned so the caller can log it (notifications can be suppressed by
// the user's Notification Center settings).
func notify(title, body string) error {
	script := "display notification " + appleScriptString(body) +
		" with title " + appleScriptString(title)
	return exec.Command("osascript", "-e", script).Run()
}

// appleScriptString renders s as a quoted AppleScript string literal, escaping
// the backslash and double-quote characters that would otherwise terminate it.
// Shared by the notification and elevated-hosts (osascript) code paths.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
