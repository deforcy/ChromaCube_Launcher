//go:build windows

package main

import (
	"sync"

	toast "git.sr.ht/~jackmordaunt/go-toast/v2"
)

var notifyOnce sync.Once

// notifyInit registers our AppID in the registry once. go-toast requires this
// (via SetAppData) before WinRT toasts will display; without it the COM push
// fails and only the PowerShell fallback (often silent) is left.
func notifyInit() {
	notifyOnce.Do(func() {
		_ = toast.SetAppData(toast.AppData{
			AppID: "ChromaCube Launcher",
			GUID:  "{a3f1c2d4-5b6e-7081-92a3-b4c5d6e7f809}",
		})
	})
}

// notify shows a Windows toast. Returns the error so the caller can log why it
// failed (toasts from an elevated process can be blocked by Windows).
func notify(title, body string) error {
	notifyInit()
	n := toast.Notification{
		AppID: "ChromaCube Launcher",
		Title: title,
		Body:  body,
	}
	return n.Push()
}
