//go:build !darwin

package main

// ensureLoopbackAlias is a no-op outside macOS: Windows and Linux both treat
// the whole 127.0.0.0/8 block as usable loopback addresses without needing
// each one explicitly aliased first. See loopback_darwin.go.
func ensureLoopbackAlias(ip string) error { return nil }
