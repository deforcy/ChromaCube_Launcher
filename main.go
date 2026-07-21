package main

import (
	"embed"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// noCacheMiddleware tells WebView2 never to cache our embedded assets, so a
// rebuilt binary's new JS/CSS always loads instead of a stale cached copy.
func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

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

	// Only one instance may run, so we never get duplicate tray icons. A post-
	// update relaunch waits briefly for the outgoing instance to release the lock.
	// A plain duplicate launch nudges the running instance to show itself, then exits.
	if !acquireInstanceLock(app.justUpdated) {
		nudgeExistingInstance()
		return
	}

	// Start hidden in the tray ONLY when launched at system logon (the autostart
	// task passes --tray). A direct launch, or a post-update relaunch, opens the
	// window. First run always shows the window so the user can enter their code.
	startHidden := app.startInTray && !app.needsCode

	// Wails owns the main goroutine / UI thread. All process management happens
	// in the App, driven by the bound methods and the lifecycle hooks below.
	err := wails.Run(&options.App{
		Title:       "ChromaCube Launcher",
		Width:       920,
		Height:      560,
		MinWidth:    720,
		MinHeight:   460,
		StartHidden: startHidden,
		AssetServer: &assetserver.Options{
			Assets:     assets,
			Middleware: noCacheMiddleware,
		},
		// Per-version WebView2 data dir: a new build starts with a fresh asset
		// cache, so updated JS/CSS can never be shadowed by a previously cached copy.
		Windows: &windows.Options{
			WebviewUserDataPath: filepath.Join(os.TempDir(), "ChromaCubeLauncher-webview-"+appVersion),
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
