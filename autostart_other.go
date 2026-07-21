//go:build !windows && !darwin

package main

// applyAutostart is a no-op on platforms without an autostart implementation
// (currently everything except Windows, which uses a logon scheduled task, and
// macOS, which uses a per-user LaunchAgent).
func applyAutostart(enable bool) error { return nil }
