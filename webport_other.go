//go:build !windows

package main

// defaultWebPort is the fallback local port for a web target (the live map)
// when config.json doesn't set webPort. Unlike Windows, macOS/Linux require
// root to bind ports below 1024, so binding port 80 as a regular user fails
// with "permission denied". Default to an unprivileged port instead; the
// branded local URL just shows the port (e.g. http://map.chromacube:8080).
const defaultWebPort = 8080
