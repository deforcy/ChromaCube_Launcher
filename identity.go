package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// loadOrCreateInstallID returns a stable, random per-install identifier, so the
// owner can correlate a user's log embeds across sessions without any PII. It is
// generated once and stored in the app data dir.
func loadOrCreateInstallID(dataDir string) string {
	path := filepath.Join(dataDir, "install-id")
	if data, err := os.ReadFile(path); err == nil {
		if id := string(data); len(id) >= 8 {
			return id
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	id := hex.EncodeToString(buf)
	_ = os.WriteFile(path, []byte(id), 0o600)
	return id
}

func accessCodePath(dataDir string) string {
	return filepath.Join(dataDir, "access-code")
}

// loadStoredCode returns the personal access code the user entered on a previous
// launch (universal build), or "" if none.
func loadStoredCode(dataDir string) string {
	data, err := os.ReadFile(accessCodePath(dataDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func storeAccessCode(dataDir, code string) {
	_ = os.WriteFile(accessCodePath(dataDir), []byte(strings.TrimSpace(code)), 0o600)
}

func clearAccessCode(dataDir string) {
	_ = os.Remove(accessCodePath(dataDir))
}

// displayNameFor picks a human label for the user/group. The remote config's
// displayName wins; otherwise we fall back to the machine hostname so devices
// are still distinguishable in the logs.
func displayNameFor(configured string) string {
	if configured != "" {
		return configured
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		return "Device " + hn
	}
	return "Unknown"
}
