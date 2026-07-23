//go:build !windows

package main

// startTray/stopTray are no-ops on non-Windows platforms (tray is Windows-only).
func (a *App) startTray() {}
func (a *App) stopTray()  {}

// backgroundHint tells the user how to get the window back after closing it.
func backgroundHint() string {
	return "Still running in the background. Click the Dock icon to bring the window back."
}
