//go:build darwin

package main

// hideWindowOnClose tells Wails to divert the window's close button straight
// to a native [NSApp hide:] (see WindowDelegate.m in Wails), bypassing
// OnBeforeClose entirely for that action. Two things fall out of this:
//
//  1. A native app-hide is exactly what makes clicking the Dock icon restore
//     the window again (AppKit's default reopen behaviour only un-hides/
//     un-miniaturises; it does not react to a plain WindowHide/orderOut).
//  2. Since the close button no longer reaches beforeClose on macOS, that
//     hook is now exclusively invoked for a genuine quit (Cmd+Q, the Dock
//     tile's "Quit", or the app menu's "Quit"), so it should let those
//     through instead of hiding again - see beforeClose in app.go.
const hideWindowOnClose = true
