//go:build darwin

package main

// cleanupHostsOnQuit is false on macOS: removing the managed hosts block on
// every quit (and re-adding it on every launch) forces a fresh Touch
// ID/password prompt at every single app start, since the block content goes
// from present to absent and back. Leaving it in place across quits means a
// launch only ever needs to elevate when the entries actually change (a new
// code, a granted/revoked server) - matching the behavior described in
// installHosts/removeHosts's doc comments. The synthetic hostnames involved
// (e.g. "chromacube") have no meaning outside this app, so a stale entry while
// the launcher isn't running has no real-world effect: nothing else resolves
// them.
const cleanupHostsOnQuit = false
