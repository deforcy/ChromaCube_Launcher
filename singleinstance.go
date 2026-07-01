package main

import (
	"net"
	"time"
)

// A loopback listener acts as a single-instance lock: only one process can hold
// the port, and the OS frees it the instant that process exits. This keeps a
// second launch from spawning a duplicate tray icon.
const instanceLockAddr = "127.0.0.1:59217"

var instanceLock net.Listener

// acquireInstanceLock reports whether this process may run. When waitForRelease
// is true (a post-update relaunch) it retries for a few seconds so the previous
// instance has time to exit and free the port before we give up.
func acquireInstanceLock(waitForRelease bool) bool {
	deadline := time.Now()
	if waitForRelease {
		deadline = deadline.Add(15 * time.Second)
	}
	for {
		ln, err := net.Listen("tcp", instanceLockAddr)
		if err == nil {
			instanceLock = ln
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// serveInstanceLock accepts connections on the lock port; each one is a second
// launch asking the running instance to surface its window. Runs until the
// listener closes (shutdown).
func serveInstanceLock(onNudge func()) {
	if instanceLock == nil {
		return
	}
	for {
		conn, err := instanceLock.Accept()
		if err != nil {
			return
		}
		conn.Close()
		if onNudge != nil {
			onNudge()
		}
	}
}

// nudgeExistingInstance tells the already-running instance to show its window,
// so a duplicate launch focuses the app instead of doing nothing.
func nudgeExistingInstance() {
	if conn, err := net.DialTimeout("tcp", instanceLockAddr, 2*time.Second); err == nil {
		conn.Close()
	}
}
