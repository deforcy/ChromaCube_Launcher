package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings holds user-tweakable preferences (the Settings tab).
type Settings struct {
	// Autostart launches the app on Windows login (default on).
	Autostart bool `json:"autostart"`
}

func defaultSettings() Settings {
	return Settings{Autostart: true}
}

func (a *App) settingsPath() string {
	return filepath.Join(a.dataDir, "settings.json")
}

// loadSettings reads settings.json, falling back to defaults when absent.
func (a *App) loadSettings() Settings {
	s := defaultSettings()
	if data, err := os.ReadFile(a.settingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func (a *App) saveSettings(s Settings) {
	if data, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(a.settingsPath(), data, 0o644)
	}
}

// ----- Bound methods --------------------------------------------------------

// GetSettings returns the current preferences for the Settings tab.
func (a *App) GetSettings() Settings {
	return a.loadSettings()
}

// SetAutostart toggles "launch on Windows login" and applies it immediately.
func (a *App) SetAutostart(enable bool) error {
	s := a.loadSettings()
	s.Autostart = enable
	a.saveSettings(s)
	return applyAutostart(enable)
}
