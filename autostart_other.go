//go:build !windows

package main

// applyAutostart is a no-op on non-Windows platforms (autostart is implemented
// via the Windows registry Run key).
func applyAutostart(enable bool) error { return nil }
