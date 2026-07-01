//go:build !windows

package main

// startTray/stopTray are no-ops on non-Windows platforms (tray is Windows-only).
func (a *App) startTray() {}
func (a *App) stopTray()  {}
