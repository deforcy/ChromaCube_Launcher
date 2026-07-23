//go:build !darwin

package main

// hideWindowOnClose is false on Windows/Linux: the close button keeps going
// through OnBeforeClose (beforeClose in app.go), which hides to the tray and
// only lets a real quit through once the tray's "Close" item (or a signal)
// has set reallyQuit. See closebehavior_darwin.go for why macOS differs.
const hideWindowOnClose = false
