//go:build darwin

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// ensureLoopbackAlias makes ip (a "127.0.0.N" loopback address) usable for
// binding. Unlike Windows/Linux, where the whole 127.0.0.0/8 block works out
// of the box, macOS only configures 127.0.0.1 on lo0 by default - a second (or
// third, ...) hostname-mode target fails to bind with "can't assign requested
// address" until its IP is explicitly aliased onto lo0. A no-op for 127.0.0.1
// itself and once an IP is already aliased (e.g. by a previous launch - the
// alias persists until reboot, so this then costs nothing on subsequent
// connects).
func ensureLoopbackAlias(ip string) error {
	if ip == "" || ip == "127.0.0.1" {
		return nil
	}
	if out, err := exec.Command("ifconfig", "lo0").CombinedOutput(); err == nil &&
		strings.Contains(string(out), "inet "+ip+" ") {
		return nil
	}
	shell := fmt.Sprintf("ifconfig lo0 alias %s up", shellQuote(ip))
	script := fmt.Sprintf("do shell script %s with administrator privileges", appleScriptString(shell))
	if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
		return fmt.Errorf("could not add loopback alias %s: %v: %s", ip, err, strings.TrimSpace(string(out)))
	}
	return nil
}
