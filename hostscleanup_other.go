//go:build !darwin

package main

// cleanupHostsOnQuit is true on Windows (and elsewhere): the whole process
// already runs elevated, so removing the managed hosts block on quit and
// re-adding it on the next launch is free - no prompt either way. Keeping
// this behavior here means no orphaned hosts entries linger when the app
// isn't running. See hostscleanup_darwin.go for why macOS differs.
const cleanupHostsOnQuit = true
