//go:build !windows

package main

// startTray is a no-op on non-Windows platforms (the tray is Windows-only here).
func (a *App) startTray() {}
