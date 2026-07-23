//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// commitHostsFile writes the new /etc/hosts contents on macOS.
//
// Editing /etc/hosts requires root. Unlike Windows - where the exe manifest
// forces a single UAC elevation at launch - a macOS .app has no way to demand
// elevation up front. So we:
//
//  1. Skip the write entirely when the contents are unchanged (no prompt).
//  2. Try a direct write, which succeeds if the app already runs as root.
//  3. Otherwise stage the new file in a temp location and copy it into place
//     with a single osascript "with administrator privileges" call, which shows
//     the native Touch ID / password dialog.
//
// The launcher only rewrites the managed block when it actually changes
// (connect, disconnect, or an access change), so this prompts at most once per
// such change rather than continuously.
func commitHostsFile(path string, data []byte) error {
	// No change -> nothing to do (and no elevation prompt).
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}

	// Fast path: already writable (running as root). O_TRUNC happens only after a
	// successful open, so a permission failure here never leaves a partial write.
	if err := os.WriteFile(path, data, 0o644); err == nil {
		return nil
	}

	// Stage the desired contents in a temp file the elevated shell can read.
	tmp, err := os.CreateTemp("", "chromacube-hosts-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Redirecting into the existing /etc/hosts (rather than mv) preserves its
	// root:wheel ownership and mode - we only replace the contents.
	shell := fmt.Sprintf("cat %s > %s", shellQuote(tmpPath), shellQuote(path))
	script := fmt.Sprintf("do shell script %s with administrator privileges", appleScriptString(shell))

	if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
		return fmt.Errorf("elevated hosts write failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// shellQuote wraps s in single quotes for safe use inside the /bin/sh command
// that osascript's "do shell script" executes, escaping any embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// appleScriptString renders s as a quoted AppleScript string literal, escaping
// the backslash and double-quote characters that would otherwise terminate it.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
