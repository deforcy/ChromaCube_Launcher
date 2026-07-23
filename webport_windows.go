//go:build windows

package main

// defaultWebPort is the fallback local port for a web target (the live map)
// when config.json doesn't set webPort. Windows has no privileged-port
// restriction for regular user sockets, so 80 just works there, keeping the
// branded local URL bare (e.g. http://map.chromacube, no ":port").
const defaultWebPort = 80
