//go:build !windows

package main

// notify is a no-op on non-Windows platforms.
func notify(title, body string) error { return nil }
