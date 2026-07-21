//go:build !windows && !darwin

package main

// notify is a no-op on platforms without a native implementation (currently
// everything except Windows and macOS).
func notify(title, body string) error { return nil }
