package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Hostname mode works by redirecting each player-facing hostname to a private
// loopback IP in the OS hosts file, where the matching cloudflared proxy is
// listening on Minecraft's default port (25565). That lets players type a bare
// "chromacube.deforce.site" with no ":port".
//
// We only ever touch the lines BETWEEN our two markers, so the rest of the
// user's hosts file is preserved untouched. Editing the hosts file requires
// administrator/root privileges (see the manifest under build/windows).

const (
	mcDefaultPort = 25565
	hostsBegin    = "# >>> ChromaCube Launcher (managed - do not edit) >>>"
	hostsEnd      = "# <<< ChromaCube Launcher (managed - do not edit) <<<"
)

type hostsEntry struct {
	IP   string
	Host string
}

// hostsPath returns the OS hosts file location.
func hostsPath() string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return filepath.Join(root, "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

// writeHostsBlock rewrites our managed block to contain exactly `entries`.
// Passing nil/empty removes the block entirely. Existing user entries outside
// the markers are left intact.
func writeHostsBlock(entries []hostsEntry) error {
	path := hostsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	nl := "\n"
	if runtime.GOOS == "windows" {
		nl = "\r\n"
	}

	// Copy through every line that isn't inside a previous managed block.
	var kept []string
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case line == hostsBegin:
			inBlock = true
		case line == hostsEnd:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}
	// Trim trailing blank lines so we don't accumulate them across rewrites.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	var b strings.Builder
	b.WriteString(strings.Join(kept, nl))
	if len(kept) > 0 {
		b.WriteString(nl)
	}
	if len(entries) > 0 {
		b.WriteString(hostsBegin + nl)
		for _, e := range entries {
			b.WriteString(e.IP + " " + e.Host + nl)
		}
		b.WriteString(hostsEnd + nl)
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}
