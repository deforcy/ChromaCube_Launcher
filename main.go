package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// assets embeds the whole frontend. The directory must contain an index.html.
// Wails serves these files and automatically injects its JS runtime and the
// Go method bindings (window.go.main.App.*) - we do not bundle them ourselves.
//
//go:embed all:frontend/dist
var assets embed.FS

// defaultConfig is the config.json baked into the binary. It is used only when
// no external config.json is found next to the executable or in the app data
// directory, so the app always has something sane to fall back to.
//
//go:embed config.json
var defaultConfig []byte

func main() {
	app := NewApp(defaultConfig)

	// Wails owns the main goroutine / UI thread. All process management happens
	// in the App, driven by the bound methods and the lifecycle hooks below.
	err := wails.Run(&options.App{
		Title:     "ChromaCube Launcher",
		Width:     920,
		Height:    560,
		MinWidth:  720,
		MinHeight: 460,
		// Start hidden in the tray once the app is configured; show the window on
		// first run so the user can enter their access code.
		StartHidden: !app.needsCode,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 17, G: 18, B: 23, A: 1},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("fatal:", err.Error())
	}
}
