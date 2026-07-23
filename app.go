package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Connection status values shared with the frontend.
const (
	StatusIdle        = "idle"
	StatusStarting    = "starting"
	StatusWaitingAuth = "waiting_auth"
	// StatusChecking means cloudflared's local listener is up but we have not yet
	// confirmed the real Minecraft server behind it answers a Server List Ping.
	StatusChecking  = "checking"
	StatusConnected = "connected"
	// StatusUnreachable means the tunnel itself is fine (cloudflared is up) but the
	// Minecraft server behind it does not answer - distinct from StatusError, which
	// means the tunnel/cloudflared side itself is broken.
	StatusUnreachable = "unreachable"
	StatusError       = "error"
	StatusStopped     = "stopped"
)

// checkConnectionFloor is the minimum time "Checking connection…" stays on
// screen before the first post-connect ping is allowed to resolve it, so a
// near-instant reply doesn't make the check look like it never happened.
const checkConnectionFloor = 700 * time.Millisecond

// urlRegexp extracts the first http(s) URL from a cloudflared log line.
var urlRegexp = regexp.MustCompile(`https?://[^\s"'<>]+`)

// App is the single Wails-bound object. It owns the cloudflared binary, the
// configured tunnels and all child-process lifecycle.
type App struct {
	ctx        context.Context
	dataDir    string
	defaultCfg []byte

	// stateMu guards the config-derived collections below (cfg, order, byID,
	// hostsEntries, hostnameMode, displayName, needsCode). They are rebuilt at
	// startup, on code submit/clear, and - crucially - mutated live by the config
	// refresh loop when the admin changes a user's access, so every reader takes
	// a snapshot under this lock.
	stateMu sync.RWMutex

	cfg   Config
	order []*tunnel          // config order, for stable UI rendering
	byID  map[string]*tunnel // id -> tunnel

	needsCode   bool   // universal build awaiting a personal access code
	reallyQuit  bool   // true once the user chose Close from the tray menu
	displayName string // who this user/group is (from remote config)
	installID   string // stable per-install id, for log correlation

	// whatsNew is the post-update "what changed" payload, captured at update time
	// (see writePendingWhatsNew) so it is shown reliably. Nil unless just updated.
	whatsNew map[string]interface{}

	updateRequired   bool   // build is older than the published version: locked
	updateURL        string // where to download the new build
	latestVersion    string // latest published version (from the manifest)
	skipHostsCleanup bool   // set during self-update: the new instance owns the hosts block

	startInTray bool // launched at system logon (--tray): start hidden in the tray
	justUpdated bool // launched right after a self-update (--updated): show what changed

	hostnameMode bool         // any target uses hosts-file redirection
	hostsEntries []hostsEntry // managed hosts entries for those targets

	log *logger // file + Discord logging (replaces the in-app log panel)

	binMu        sync.Mutex
	binaryReady  bool
	binaryStatus string
}

// tunnel tracks one configured target and the cloudflared process serving it.
type tunnel struct {
	Target
	ID string

	// Resolved local binding, computed once at startup.
	HostnameMode bool   // true when McHost is set (hosts-file redirection)
	BindIP       string // 127.0.0.1, 127.0.0.2, … (one per hostname-mode target)
	BindPort     int    // 25565 in hostname mode, else LocalPort

	// lc guards the process lifecycle fields below.
	lc       sync.Mutex
	running  bool
	stopping bool // true while WE asked it to stop, so exit isn't treated as error
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	ctrl     *processController
	websrv   *http.Server // web targets: the in-process reverse proxy (no cloudflared)

	// st guards the observable status fields below.
	st        sync.Mutex
	status    string
	message   string
	lastErr   string
	discarded bool // removed by a live config change: suppress further UI events
}

// TargetState is the snapshot sent to the UI for one target.
type TargetState struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Hostname  string `json:"hostname"`
	Protocol  string `json:"protocol"`
	LocalPort int    `json:"localPort"`
	LocalAddr string `json:"localAddr"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Web       bool   `json:"web"`       // web target: opened in a browser, not Minecraft
	WebURL    string `json:"webURL"`    // http URL to open for a web target (else "")
	CoupledTo string `json:"coupledTo"` // web target: Hostname of the server it embeds into
}

// AppState is the full snapshot the UI requests on load.
type AppState struct {
	BinaryReady  bool          `json:"binaryReady"`
	BinaryStatus string        `json:"binaryStatus"`
	NeedsCode    bool          `json:"needsCode"`   // show the access-code entry screen
	CodeMode     bool          `json:"codeMode"`    // build identifies users by typed code
	DisplayName  string        `json:"displayName"` // current user/group label
	Targets      []TargetState `json:"targets"`

	UpdateRequired bool   `json:"updateRequired"` // lock the UI behind the update screen
	UpdateURL      string `json:"updateURL"`      // download link for the new build
	LatestVersion  string `json:"latestVersion"`  // latest published version
	AppVersion     string `json:"appVersion"`     // this build's version

	WhatsNew map[string]interface{} `json:"whatsNew,omitempty"` // post-update notes, if any
}

// NewApp builds the App, resolving the data dir and loading config up front so
// that a bad config is reported at startup rather than on first click.
func NewApp(defaultCfg []byte) *App {
	a := &App{
		defaultCfg:   defaultCfg,
		byID:         map[string]*tunnel{},
		binaryStatus: "Checking cloudflared…",
		startInTray:  hasArg("--tray"),
		justUpdated:  hasArg("--updated"),
	}

	base, err := os.UserConfigDir()
	if err != nil {
		base, _ = os.Getwd()
	}
	a.dataDir = filepath.Join(base, "ChromaCubeLauncher")
	_ = os.MkdirAll(a.dataDir, 0o755)
	a.installID = loadOrCreateInstallID(a.dataDir)

	// Universal build with no code yet: defer config until the user enters one.
	code := a.resolvedCode()
	if code == "" && codeMode() && !exeAdjacentConfigExists() {
		a.needsCode = true
		a.displayName = displayNameFor("")
		a.log = newLogger(a.dataDir, "", buildSessionInfo(a.displayName, a.installID))
		return a
	}

	cfg, err := loadConfig(a.dataDir, defaultCfg, code)
	if err != nil {
		// Surface the config error through a single placeholder target so the
		// user sees something actionable instead of an empty window.
		a.cfg = Config{}
		a.displayName = displayNameFor("")
		a.log = newLogger(a.dataDir, "", buildSessionInfo(a.displayName, a.installID))
		t := &tunnel{ID: "config-error", status: StatusError,
			message: "Config error: " + err.Error()}
		a.order = []*tunnel{t}
		a.byID[t.ID] = t
		return a
	}
	a.applyConfig(cfg)
	return a
}

// resolvedCode is the access code in effect: a baked-in buildCode wins, else the
// code the user typed on a previous launch (universal build), else empty.
func (a *App) resolvedCode() string {
	if buildCode != "" {
		return buildCode
	}
	return loadStoredCode(a.dataDir)
}

// applyConfig (re)builds all per-config state: logger, tunnels and hosts entries.
// Safe to call again later (e.g. after the user submits a code), replacing any
// previous configuration. The caller is responsible for starting the logger.
func (a *App) applyConfig(cfg Config) {
	if a.log != nil {
		a.log.close()
	}
	a.log = newLogger(a.dataDir, cfg.DiscordWebhook, buildSessionInfo(displayNameFor(cfg.DisplayName), a.installID))

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	a.cfg = cfg
	a.displayName = displayNameFor(cfg.DisplayName)
	a.order = nil
	a.byID = map[string]*tunnel{}
	a.hostsEntries = nil
	loopIdx := 0
	for i, tgt := range cfg.Targets {
		t := &tunnel{
			Target:  tgt,
			ID:      fmt.Sprintf("t%d", i),
			status:  StatusIdle,
			message: "Not connected",
		}
		if tgt.Web {
			// Web targets open in a browser, which resolves *.localhost to loopback
			// natively (RFC 6761) - bypassing the hosts file AND Secure DNS/DoH,
			// neither of which a made-up hosts name survives for XHR/fetch. So we
			// bind 127.0.0.1 (where *.localhost points) and write NO hosts entry.
			t.BindIP = "127.0.0.1"
			t.BindPort = mcBindPort(tgt)
		} else if tgt.McHost != "" {
			// Hostname mode: give each target its own loopback IP (127.0.0.1,
			// 127.0.0.2, ...) and bind on Minecraft's default port so the player
			// types a bare hostname. The hosts file maps McHost -> this IP.
			t.HostnameMode = true
			t.BindIP = fmt.Sprintf("127.0.0.%d", loopIdx+1)
			t.BindPort = mcBindPort(tgt)
			loopIdx++
			a.hostsEntries = append(a.hostsEntries, hostsEntry{IP: t.BindIP, Host: tgt.McHost})
		} else {
			t.BindIP = "127.0.0.1"
			t.BindPort = tgt.LocalPort
		}
		a.order = append(a.order, t)
		a.byID[t.ID] = t
	}
	a.hostnameMode = len(a.hostsEntries) > 0
}

// mcBindPort is the local port a hostname-mode target binds: its web port for a
// web target, otherwise Minecraft's default port.
func mcBindPort(t Target) int {
	if t.Web {
		if t.WebPort > 0 {
			return t.WebPort
		}
		return defaultWebPort
	}
	return mcDefaultPort
}

// snapshotOrder returns a copy of the current tunnel order safe to range over
// without holding the lock (the underlying tunnels are shared pointers).
func (a *App) snapshotOrder() []*tunnel {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	out := make([]*tunnel, len(a.order))
	copy(out, a.order)
	return out
}

// lookup resolves a tunnel by id under the state lock.
func (a *App) lookup(id string) *tunnel {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.byID[id]
}

// isNeedsCode reports whether the app is currently waiting for an access code.
func (a *App) isNeedsCode() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.needsCode
}

// ----- Wails lifecycle hooks -------------------------------------------------

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// Remove the previous binary left behind by a self-update swap (if any).
	go cleanupOldUpdate()

	a.log.start()

	// System tray (close-to-tray, start-in-tray). No-op on non-Windows.
	a.startTray()

	// Surface the window if a second launch nudges us via the instance lock.
	go serveInstanceLock(a.showWindow)

	// Live per-server ping (Minecraft Server List Ping through the tunnel).
	go a.pingLoop()

	// Re-check the published version periodically so a running app locks itself
	// when a new version ships, without needing a restart.
	go a.updateCheckLoop()

	// Re-fetch this user's config on an interval so access changes made in the
	// admin panel (a server granted or revoked) take effect live - including
	// force-disconnecting a server the user just lost access to.
	go a.configRefreshLoop()

	// If we were just relaunched by a self-update, show what changed. Load the
	// notes captured at update time (synchronously, so GetState already carries
	// them when the UI first asks), then also push them as an event.
	if a.justUpdated {
		a.loadPendingWhatsNew()
		go a.announceUpdate()
	}

	// Keep the Windows autostart entry in sync with the saved preference.
	_ = applyAutostart(a.loadSettings().Autostart)

	// Resolve public IP/geo (best effort), then announce the session (a no-op
	// while we are still waiting for a code, since there is no webhook yet).
	go func() {
		a.log.resolveLocation()
		if !a.needsCode {
			a.log.sessionStart(a.serverLabels())
		}
	}()

	// Honour Ctrl+C / SIGTERM when the app is launched from a terminal, so we
	// still tear down child processes AND restore the hosts file.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		a.reallyQuit = true // a signal means really exit, not hide to tray
		a.DisconnectAll()
		a.removeHosts()
		runtime.Quit(a.ctx)
	}()

	// Install the hosts-file redirections for hostname-mode targets (only once we
	// have a config; otherwise this happens when the user submits their code).
	// Runs in its own goroutine: on macOS this can block on a Touch ID/password
	// prompt, and cloudflared binds its local listener directly by IP (not by
	// the branded hostname), so nothing below needs to wait for it - without
	// this, a slow/unnoticed prompt stalls checkUpdate/ensureBinary/ConnectAll
	// too, leaving the UI stuck on "Checking cloudflared…" indefinitely.
	if !a.needsCode {
		go a.installHosts()
	}

	// Check for a forced update first. If this build is behind, lock the UI and
	// skip downloading cloudflared / connecting until the user installs the new
	// version. Otherwise fetch cloudflared in the background (the UI stays usable
	// and reflects progress through "binary" events) and auto-connect once ready.
	go func() {
		a.checkUpdate()
		if a.updateRequired {
			return
		}
		if err := a.ensureBinary(); err != nil {
			a.emitBinary("error", 0, err.Error())
			return
		}
		if !a.needsCode {
			a.ConnectAll()
		}
	}()
}

// checkUpdate fetches the published version and, if this build is behind, flips
// the app into a locked "update required" state and tells the UI. Best effort:
// a failed or blank check leaves the app fully usable.
func (a *App) checkUpdate() {
	info, err := fetchUpdateInfo()
	if err != nil {
		return
	}
	a.latestVersion = info.Latest
	newlyRequired := false
	if isUpdateRequired(appVersion, info.Latest) {
		newlyRequired = !a.updateRequired
		a.updateRequired = true
		a.updateURL = info.URL
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "update", map[string]interface{}{
			"required": a.updateRequired,
			"url":      a.updateURL,
			"latest":   a.latestVersion,
			"current":  appVersion,
		})
	}
	// If we only just fell behind mid-session, drop the tunnels so the lock is
	// enforced rather than cosmetic, and surface the window so the user sees it.
	if newlyRequired {
		a.DisconnectAll()
		if a.ctx != nil {
			runtime.Show(a.ctx)
		}
	}
}

// RecheckUpdate re-queries the version endpoint. The update screen calls this
// after the user installs the new build, so a still-running old instance can
// notice it is now current. (Normally they just relaunch the new exe.)
func (a *App) RecheckUpdate() AppState {
	a.checkUpdate()
	return a.GetState()
}

// SelfUpdate downloads the latest build, swaps it in place of the running exe,
// and relaunches - all on demand from the update screen. Windows lets us rename
// a running executable, so we move the current exe aside, drop the new one into
// its place, start it, and quit. The old file is cleaned up on the next launch.
func (a *App) SelfUpdate() error {
	info, err := fetchUpdateInfo()
	if err != nil {
		return fmt.Errorf("could not check for the update: %w", err)
	}
	if info.URL == "" {
		return fmt.Errorf("no download is configured for this version yet")
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.Abs(self)
	dir := filepath.Dir(self)
	newPath := filepath.Join(dir, newUpdateName)
	oldPath := filepath.Join(dir, oldUpdateName)

	a.emitUpdateProgress("Downloading the new version...", true)
	if err := downloadFile(info.URL, newPath); err != nil {
		a.emitUpdateProgress("", false)
		return fmt.Errorf("download failed: %w", err)
	}
	if err := verifySHA256(newPath, info.SHA256); err != nil {
		_ = os.Remove(newPath)
		a.emitUpdateProgress("", false)
		return fmt.Errorf("the downloaded file failed verification: %w", err)
	}

	a.emitUpdateProgress("Installing...", true)
	_ = os.Remove(oldPath)
	if err := os.Rename(self, oldPath); err != nil {
		_ = os.Remove(newPath)
		a.emitUpdateProgress("", false)
		return fmt.Errorf("could not replace the current version: %w", err)
	}
	if err := os.Rename(newPath, self); err != nil {
		_ = os.Rename(oldPath, self) // best-effort rollback
		a.emitUpdateProgress("", false)
		return fmt.Errorf("could not install the new version: %w", err)
	}

	// Capture the notes for THIS update now, while we still have the manifest in
	// hand, so the relaunched build shows them reliably (no re-fetch/race).
	a.writePendingWhatsNew(info.Notes)

	a.emitUpdateProgress("Restarting...", true)
	// Relaunch visibly (no --tray) and flag it so the new copy shows what changed.
	if err := relaunch(self, "--updated"); err != nil {
		a.emitUpdateProgress("", false)
		return fmt.Errorf("updated, but could not relaunch - please start the app again: %w", err)
	}
	// Remove our tray icon now so the outgoing instance does not leave a ghost.
	a.stopTray()

	// Hand off to the new instance: leave the hosts block in place (the new
	// process owns it now) and really quit.
	a.skipHostsCleanup = true
	a.reallyQuit = true
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
	return nil
}

func (a *App) emitUpdateProgress(message string, busy bool) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "updateProgress", map[string]interface{}{
		"message": message,
		"busy":    busy,
	})
}

// pingLoop measures latency to each running server every few seconds via a
// Server List Ping and emits "ping" events ({id, ms, motd, ...}; ms = -1 means
// unreachable). It also treats a successful ping as ground truth that the server
// is reachable: if the tunnel got stuck showing an error (or "unreachable")
// after a transient hiccup, a good ping clears it back to "connected".
func (a *App) pingLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, t := range a.snapshotOrder() {
			// Web targets speak HTTP, not the Minecraft Server List Ping - skip them.
			if t.Web {
				continue
			}
			st := t.currentStatus()
			// Ping while the local listener is up (connected/checking) or wrongly
			// stuck in error/unreachable - the ping is what tells us which one it
			// really is.
			if !t.isRunning() || (st != StatusConnected && st != StatusChecking &&
				st != StatusError && st != StatusUnreachable) {
				continue
			}
			go a.pingOnce(t)
		}
	}
}

// pingOnce performs a single Server List Ping against t, emits the resulting
// "ping" event ({id, ms, motd, ...}; ms = -1 means unreachable), and updates
// t's status to match reality:
//   - a successful ping while checking/error/unreachable promotes to Connected
//     (the real server answered, so any earlier problem has cleared)
//   - a failed ping while connected/checking demotes to Unreachable (cloudflared's
//     tunnel is fine, but the Minecraft server behind it is not answering)
func (a *App) pingOnce(t *tunnel) {
	status, err := pingServer(t.localAddr())
	if !t.isRunning() {
		return
	}
	payload := map[string]interface{}{"id": t.ID, "ms": -1}
	if err == nil {
		payload["ms"] = status.Ms
		payload["motd"] = status.Motd
		payload["playersOnline"] = status.PlayersOn
		payload["playersMax"] = status.PlayersMax
		payload["version"] = status.Version
		payload["favicon"] = status.Favicon
		switch t.currentStatus() {
		case StatusError, StatusUnreachable, StatusChecking:
			t.setStatus(a, StatusConnected, "Connected")
		}
	} else {
		switch t.currentStatus() {
		case StatusConnected, StatusChecking:
			t.setStatus(a, StatusUnreachable, "Unreachable")
		}
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "ping", payload)
	}
}

// showWindow brings the hidden (tray) window back to the foreground and re-checks
// for a new version (so reopening from the tray behaves like a fresh check).
func (a *App) showWindow() {
	if a.ctx != nil {
		runtime.Show(a.ctx)
		go a.checkUpdate()
	}
}

// updateCheckLoop re-checks the published version on an interval. Once the app is
// locked there is nothing more to do, so it stops.
func (a *App) updateCheckLoop() {
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		if a.updateRequired {
			return
		}
		a.checkUpdate()
	}
}

// configRefreshInterval is how often a running app re-fetches its config so that
// access changes (a server granted or revoked) take effect without a restart.
const configRefreshInterval = 30 * time.Second

// configRefreshLoop periodically re-fetches this user's config from the Worker
// and reconciles it against the running tunnels. Granting a server makes it
// appear; dropping one force-disconnects it; a fully revoked code (HTTP 404)
// sends the user back to the code screen. Only meaningful when we have a code
// (universal builds, or a baked group code). Transient network errors are
// ignored - we keep the current config until the next tick.
func (a *App) configRefreshLoop() {
	// Never poll faster than this; the endpoint is cheap but this is background.
	ticker := time.NewTicker(configRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		if a.updateRequired || a.isNeedsCode() {
			continue
		}
		code := a.resolvedCode()
		if code == "" {
			continue
		}
		cfg, err := fetchRemoteConfig(a.dataDir, code)
		if err != nil {
			if errors.Is(err, errCodeRevoked) {
				a.handleRevoked()
			}
			continue // transient errors: keep the current config
		}
		a.reconcileConfig(cfg)
	}
}

// reconcileConfig diffs a freshly fetched config against the running tunnels and
// applies the difference in place: tunnels for dropped targets are stopped and
// removed, tunnels for new targets are added (and started if the network is
// currently up), and unchanged targets are left running untouched. It also keeps
// the hosts file in sync with the new set.
func (a *App) reconcileConfig(cfg Config) {
	a.stateMu.Lock()

	oldByKey := map[string]*tunnel{}
	for _, t := range a.order {
		oldByKey[targetKey(t.Target)] = t
	}

	// Decide what to keep, and remember which loopback octets kept hostname-mode
	// tunnels occupy so newly added ones don't collide with a live listener.
	wantKeys := map[string]bool{}
	kept := map[string]*tunnel{}
	usedOctet := map[int]bool{}
	for _, tgt := range cfg.Targets {
		k := targetKey(tgt)
		wantKeys[k] = true
		if ex, ok := oldByKey[k]; ok {
			kept[k] = ex
			if ex.HostnameMode {
				usedOctet[octetOf(ex.BindIP)] = true
			}
		}
	}

	nextOctet := func() int {
		for i := 1; ; i++ {
			if !usedOctet[i] {
				usedOctet[i] = true
				return i
			}
		}
	}
	nextID := a.maxTunnelNumLocked()

	var newOrder []*tunnel
	var toStart []*tunnel
	for _, tgt := range cfg.Targets {
		k := targetKey(tgt)
		if ex, ok := kept[k]; ok {
			newOrder = append(newOrder, ex)
			continue
		}
		nextID++
		t := &tunnel{
			Target:  tgt,
			ID:      fmt.Sprintf("t%d", nextID),
			status:  StatusIdle,
			message: "Not connected",
		}
		if tgt.Web {
			// See applyConfig: web targets use *.localhost + 127.0.0.1, no hosts entry.
			t.BindIP = "127.0.0.1"
			t.BindPort = mcBindPort(tgt)
		} else if tgt.McHost != "" {
			t.HostnameMode = true
			t.BindIP = fmt.Sprintf("127.0.0.%d", nextOctet())
			t.BindPort = mcBindPort(tgt)
		} else {
			t.BindIP = "127.0.0.1"
			t.BindPort = tgt.LocalPort
		}
		newOrder = append(newOrder, t)
		toStart = append(toStart, t)
	}

	var toStop []*tunnel
	for _, t := range a.order {
		if !wantKeys[targetKey(t.Target)] {
			toStop = append(toStop, t)
		}
	}

	// If nothing structural changed, bail without touching anything.
	if len(toStop) == 0 && len(toStart) == 0 {
		a.stateMu.Unlock()
		return
	}

	// Swap in the new collections + hosts entries.
	a.order = newOrder
	a.byID = map[string]*tunnel{}
	for _, t := range newOrder {
		a.byID[t.ID] = t
	}
	a.cfg.Targets = cfg.Targets
	if cfg.DisplayName != "" {
		a.displayName = displayNameFor(cfg.DisplayName)
	}
	a.hostsEntries = nil
	for _, t := range newOrder {
		if t.HostnameMode {
			a.hostsEntries = append(a.hostsEntries, hostsEntry{IP: t.BindIP, Host: t.McHost})
		}
	}
	a.hostnameMode = len(a.hostsEntries) > 0
	networkOn := false
	for _, t := range newOrder {
		if t.isRunning() {
			networkOn = true
			break
		}
	}
	a.stateMu.Unlock()

	// Apply outside the lock (start/stop block on process I/O).
	for _, t := range toStop {
		// Discard first so the async "stopped" from stop() is swallowed and can't
		// re-create the card we're about to remove.
		t.markDiscarded()
		t.stop(a)
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "removed", map[string]string{"id": t.ID})
		}
	}
	// Rewrite (or clear) the managed hosts block for the new set.
	if a.hostnameMode {
		a.installHosts()
	} else {
		a.removeHosts()
	}
	for _, t := range toStart {
		if networkOn {
			_ = t.start(a) // start() emits its own "status" event, rendering the card
		} else if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "status", t.state()) // just show it idle
		}
	}
	if a.log != nil {
		a.log.line("system", "config",
			fmt.Sprintf("live access update: +%d server(s), -%d server(s)", len(toStart), len(toStop)))
	}
}

// handleRevoked responds to the Worker reporting this code is no longer valid:
// drop every tunnel and send the user back to the access-code screen with a
// notice. For a baked group build (no code screen) we simply disconnect and
// surface the state through the log.
func (a *App) handleRevoked() {
	if a.isNeedsCode() {
		return // already back at the code screen (nothing more to do)
	}
	// Baked group build: there is no code screen, so just make sure the tunnels
	// are down. Only act (and log) when something is actually running, so a
	// permanently-revoked group build doesn't spam the log every poll.
	if !codeMode() {
		running := false
		for _, t := range a.snapshotOrder() {
			if t.isRunning() {
				running = true
				break
			}
		}
		if running {
			if a.log != nil {
				a.log.line("system", "access", "access was revoked by the admin")
			}
			a.DisconnectAll()
		}
		return
	}
	if a.log != nil {
		a.log.line("system", "access", "access was revoked by the admin")
	}
	a.resetToCodeEntry()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "revoked",
			map[string]string{"message": "Your access was revoked. Enter a new code to continue."})
		runtime.EventsEmit(a.ctx, "reloaded", a.GetState())
		runtime.Show(a.ctx)
	}
}

// maxTunnelNumLocked returns the highest N among "tN" tunnel ids currently in
// a.order (or -1 if none), so reconcile can mint fresh, non-colliding ids. Must
// be called with stateMu held.
func (a *App) maxTunnelNumLocked() int {
	max := -1
	for _, t := range a.order {
		if strings.HasPrefix(t.ID, "t") {
			if n, err := strconv.Atoi(t.ID[1:]); err == nil && n > max {
				max = n
			}
		}
	}
	return max
}

// targetKey is a stable identity for a target across config refreshes, so an
// unchanged target keeps its running tunnel while genuinely new/removed ones are
// detected.
func targetKey(t Target) string {
	return strings.Join([]string{
		t.Protocol, t.Hostname, t.McHost,
		strconv.Itoa(t.LocalPort), strconv.FormatBool(t.Web),
	}, "|")
}

// octetOf returns the last octet of a "127.0.0.N" loopback IP (0 if unparsable).
func octetOf(ip string) int {
	if i := strings.LastIndexByte(ip, '.'); i >= 0 {
		if n, err := strconv.Atoi(ip[i+1:]); err == nil {
			return n
		}
	}
	return 0
}

// pendingWhatsNewName is the file the outgoing updater drops the release notes
// into, so the freshly relaunched build can show them without any network call.
const pendingWhatsNewName = "pending-whatsnew.json"

// writePendingWhatsNew records the notes for the build we are about to install,
// so the new instance shows exactly them (no re-fetch, no KV-consistency lag, no
// event-timing race). Called by SelfUpdate right before relaunch.
func (a *App) writePendingWhatsNew(notes string) {
	if strings.TrimSpace(notes) == "" {
		return
	}
	data, err := json.Marshal(map[string]string{"notes": notes})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(a.dataDir, pendingWhatsNewName), data, 0o644)
}

// loadPendingWhatsNew consumes the notes file left by the previous (updating)
// instance into a.whatsNew. It is a one-shot: the file is deleted immediately.
func (a *App) loadPendingWhatsNew() {
	path := filepath.Join(a.dataDir, pendingWhatsNewName)
	data, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return
	}
	var p struct {
		Notes string `json:"notes"`
	}
	if json.Unmarshal(data, &p) != nil || strings.TrimSpace(p.Notes) == "" {
		return
	}
	a.stateMu.Lock()
	a.whatsNew = map[string]interface{}{"version": appVersion, "notes": p.Notes}
	a.stateMu.Unlock()
}

// announceUpdate pushes the "what changed" popup after a self-update. It prefers
// the notes captured at update time (loadPendingWhatsNew); if none were captured
// (e.g. updated from a build predating that mechanism) it falls back to the live
// manifest, then the repo's GitHub release notes. The version shown is this
// build's baked appVersion.
func (a *App) announceUpdate() {
	a.stateMu.RLock()
	payload := a.whatsNew
	a.stateMu.RUnlock()

	if payload == nil {
		notes := ""
		if info, err := fetchUpdateInfo(); err == nil {
			notes = strings.TrimSpace(info.Notes)
		}
		tag := ""
		if notes == "" {
			tag, notes = fetchLatestRelease()
		}
		payload = map[string]interface{}{"version": appVersion, "tag": tag, "notes": notes}
		a.stateMu.Lock()
		a.whatsNew = payload
		a.stateMu.Unlock()
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "updated", payload)
	}
}

// hasArg reports whether the process was launched with the given flag.
func hasArg(flag string) bool {
	for _, a := range os.Args[1:] {
		if a == flag {
			return true
		}
	}
	return false
}

// quitApp performs a real quit (from the tray "Close" item), as opposed to the
// window-close button which only hides to tray.
func (a *App) quitApp() {
	a.reallyQuit = true
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// beforeClose runs on the UI thread for a close/quit attempt. On Windows this
// fires for the window's close button too, so we hide to the tray and keep
// running unless the user explicitly chose "Close" from the tray menu
// (reallyQuit). On macOS, hideWindowOnClose diverts the close button straight
// to a native app-hide (see closebehavior_darwin.go), so by the time this hook
// runs it is always a genuine quit (Cmd+Q, Dock "Quit", or the app menu) - let
// it proceed instead of hiding an already-hidden app.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.reallyQuit || hideWindowOnClose {
		return false
	}
	runtime.Hide(ctx)
	go func() {
		if err := notify("ChromaCube Launcher", backgroundHint()); err != nil {
			a.log.line("system", "notify", "notification failed: "+err.Error())
		}
	}()
	return true
}

// shutdown is the final hook; the calls are idempotent so running them again
// is harmless and guarantees no orphaned processes or stale hosts entries
// survive us.
func (a *App) shutdown(ctx context.Context) {
	a.stopTray() // remove the tray icon so it does not linger after we exit
	a.DisconnectAll()
	// During a self-update the freshly launched instance owns the hosts block, so
	// the outgoing process must not strip it out from under the new one. On
	// platforms where cleaning up costs nothing (Windows, already elevated) we
	// still do it so no orphaned entry lingers; where it would cost a fresh
	// elevation prompt on every future launch (macOS) we leave it in place - see
	// cleanupHostsOnQuit.
	if !a.skipHostsCleanup && cleanupHostsOnQuit {
		a.removeHosts()
	}
	a.log.sessionEnd()
	a.log.close()
}

// serverLabels lists the configured server names for the session-start embed.
func (a *App) serverLabels() []string {
	order := a.snapshotOrder()
	labels := make([]string, 0, len(order))
	for _, t := range order {
		if t.ID == "config-error" {
			continue
		}
		labels = append(labels, t.Label)
	}
	return labels
}

// installHosts writes the managed hosts block for hostname-mode targets. A
// permission error here almost always means the app isn't elevated; we surface
// a clear, actionable message rather than failing silently.
func (a *App) installHosts() {
	if !a.hostnameMode {
		return
	}
	if err := writeHostsBlock(a.hostsEntries); err != nil {
		a.emitHostsError("Could not update the hosts file: " + err.Error() +
			". Run the launcher as administrator so players can connect by hostname.")
	}
}

// removeHosts strips our managed block, restoring the user's hosts file.
func (a *App) removeHosts() {
	if !a.hostnameMode {
		return
	}
	_ = writeHostsBlock(nil)
}

// ----- Bound methods (callable from JS as window.go.main.App.*) --------------

// GetState returns the full current snapshot for initial UI render.
func (a *App) GetState() AppState {
	a.binMu.Lock()
	ready, bs := a.binaryReady, a.binaryStatus
	a.binMu.Unlock()

	a.stateMu.RLock()
	needsCode, displayName := a.needsCode, a.displayName
	whatsNew := a.whatsNew
	targets := make([]TargetState, 0, len(a.order))
	for _, t := range a.order {
		targets = append(targets, t.state())
	}
	a.stateMu.RUnlock()

	return AppState{
		BinaryReady:  ready,
		BinaryStatus: bs,
		NeedsCode:    needsCode,
		CodeMode:     codeMode(),
		DisplayName:  displayName,
		Targets:      targets,

		UpdateRequired: a.updateRequired,
		UpdateURL:      a.updateURL,
		LatestVersion:  a.latestVersion,
		AppVersion:     appVersion,
		WhatsNew:       whatsNew,
	}
}

// SubmitAccessCode validates and stores a user-entered access code (universal
// build), loads that user's config, and brings the launcher fully online.
func (a *App) SubmitAccessCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("please enter your access code")
	}
	cfg, err := fetchRemoteConfig(a.dataDir, code)
	if err != nil {
		return fmt.Errorf("that code did not work: %w", err)
	}
	storeAccessCode(a.dataDir, code)

	a.applyConfig(cfg)
	a.stateMu.Lock()
	a.needsCode = false
	a.stateMu.Unlock()
	a.log.start()
	go func() {
		a.log.resolveLocation()
		a.log.sessionStart(a.serverLabels())
	}()
	a.installHosts()

	runtime.EventsEmit(a.ctx, "reloaded", a.GetState())
	return nil
}

// ClearAccessCode forgets the stored code and returns to the entry screen
// (only meaningful for a universal/code-mode build).
func (a *App) ClearAccessCode() {
	if !codeMode() {
		return
	}
	a.resetToCodeEntry()
	runtime.EventsEmit(a.ctx, "reloaded", a.GetState())
}

// resetToCodeEntry tears down all tunnels, forgets the stored code, and returns
// the app to the access-code entry state. Shared by the manual "Change code"
// action and the automatic revocation path. It does NOT emit "reloaded" - the
// caller decides what to tell the UI (a plain reload vs. a revocation notice).
func (a *App) resetToCodeEntry() {
	a.DisconnectAll()
	a.removeHosts()
	clearAccessCode(a.dataDir)
	if a.log != nil {
		a.log.close()
	}

	a.stateMu.Lock()
	a.needsCode = true
	a.cfg = Config{}
	a.order = nil
	a.byID = map[string]*tunnel{}
	a.hostsEntries = nil
	a.hostnameMode = false
	a.displayName = displayNameFor("")
	a.stateMu.Unlock()

	a.log = newLogger(a.dataDir, "", buildSessionInfo(a.displayName, a.installID))
	a.log.start()
}

// Connect starts the tunnel for a single target id.
func (a *App) Connect(id string) error {
	if err := a.ensureBinary(); err != nil {
		return fmt.Errorf("cloudflared not available: %w", err)
	}
	t := a.lookup(id)
	if t == nil {
		return fmt.Errorf("unknown target %q", id)
	}
	return t.start(a)
}

// Disconnect stops the tunnel for a single target id.
func (a *App) Disconnect(id string) error {
	t := a.lookup(id)
	if t == nil {
		return fmt.Errorf("unknown target %q", id)
	}
	t.stop(a)
	return nil
}

// OpenWeb opens a web target (e.g. the live map) in the user's default browser.
func (a *App) OpenWeb(id string) error {
	t := a.lookup(id)
	if t == nil {
		return fmt.Errorf("unknown target %q", id)
	}
	if !t.Web {
		return fmt.Errorf("%q is not a web target", t.Label)
	}
	if a.ctx != nil {
		runtime.BrowserOpenURL(a.ctx, t.webURL())
	}
	return nil
}

// ConnectAll starts every configured tunnel.
func (a *App) ConnectAll() {
	if a.updateRequired {
		return // locked behind the forced-update screen
	}
	if err := a.ensureBinary(); err != nil {
		a.emitBinary("error", 0, err.Error())
		return
	}
	for _, t := range a.snapshotOrder() {
		_ = t.start(a)
	}
}

// DisconnectAll stops every configured tunnel. Safe to call multiple times.
func (a *App) DisconnectAll() {
	for _, t := range a.snapshotOrder() {
		t.stop(a)
	}
}

// ----- tunnel process management --------------------------------------------

// localAddr is what cloudflared binds to locally (the --url value).
func (t *tunnel) localAddr() string { return fmt.Sprintf("%s:%d", t.BindIP, t.BindPort) }

// displayAddr is what the player types into Minecraft.
func (t *tunnel) displayAddr() string {
	if t.HostnameMode {
		return t.McHost // e.g. "chromacube.deforce.site" (no port needed)
	}
	return fmt.Sprintf("localhost:%d", t.BindPort)
}

// webURL is the browser URL for a web target, e.g. "http://map.chromacube"
// (bare, since it binds port 80) or "http://<host>:<port>" otherwise.
func (t *tunnel) webURL() string {
	host := t.McHost
	if host == "" {
		host = t.BindIP
	}
	if t.BindPort != 80 && t.BindPort != 0 {
		host = fmt.Sprintf("%s:%d", host, t.BindPort)
	}
	return "http://" + host
}

// start launches `cloudflared access <tcp|udp> --hostname H --url 127.0.0.1:PORT`.
// It is idempotent: a second call while running is a no-op.
func (t *tunnel) start(a *App) error {
	// Guard against the synthetic config-error placeholder (no real target).
	if t.Hostname == "" || t.Protocol == "" {
		return nil
	}

	// Web targets (the live map) are HTTP, not raw TCP: cloudflared's `access tcp`
	// bridges a TCP stream, which an HTTP origin can't complete (the WebSocket
	// bridge handshake fails). Serve them with an in-process reverse proxy instead.
	if t.Web {
		return t.startWeb(a)
	}

	t.lc.Lock()
	defer t.lc.Unlock()
	if t.running {
		return nil
	}

	t.setStatus(a, StatusStarting, "Starting cloudflared…")
	t.setLastErr("")

	// Hostname-mode targets past the first each get their own loopback IP
	// (127.0.0.2, ...); on macOS that address must be aliased onto lo0 before
	// anything can bind it (see ensureLoopbackAlias), unlike Windows/Linux
	// where the whole 127.0.0.0/8 block just works.
	if err := ensureLoopbackAlias(t.BindIP); err != nil {
		t.setStatus(a, StatusError, err.Error())
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	args := []string{
		"access", t.Protocol,
		"--hostname", t.Hostname,
		"--url", t.localAddr(),
	}
	cmd := exec.CommandContext(ctx, a.binaryPath(), args...)

	// If a Cloudflare Access service token is configured, hand it to cloudflared
	// via environment variables (not argv, so the secret never shows up in the
	// process list). This authenticates headlessly; without it cloudflared falls
	// back to the interactive browser login.
	// Pass the service token via env, not argv, so it never shows in the process list.
	cmd.Env = os.Environ()
	if t.ServiceTokenID != "" && t.ServiceTokenSecret != "" {
		cmd.Env = append(cmd.Env,
			"TUNNEL_SERVICE_TOKEN_ID="+t.ServiceTokenID,
			"TUNNEL_SERVICE_TOKEN_SECRET="+t.ServiceTokenSecret,
		)
	}

	t.ctrl = newProcessController()
	t.ctrl.prepare(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.setStatus(a, StatusError, err.Error())
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.setStatus(a, StatusError, err.Error())
		return err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		t.setStatus(a, StatusError, "failed to launch cloudflared: "+err.Error())
		return err
	}
	// On Windows this assigns the process to a kill-on-close job object; on Unix
	// it is a no-op (the process group is configured in prepare()).
	if err := t.ctrl.started(cmd); err != nil {
		a.log.line(t.Label, "stderr", "warning: process-group setup failed: "+err.Error())
	}

	t.cmd = cmd
	t.cancel = cancel
	t.running = true
	t.stopping = false

	// cloudflared logs to both streams; treat them the same.
	go t.readPipe(a, stdout, "stdout")
	go t.readPipe(a, stderr, "stderr")
	go t.wait(a)
	return nil
}

// startWeb serves a web target (the live map) with an in-process HTTP reverse
// proxy instead of cloudflared. It listens on the same loopback IP:port a
// cloudflared tunnel would (so the hosts-file redirect and Open Map URL are
// unchanged) and forwards every request to https://<Hostname>, injecting the
// Access service-token headers - exactly what authenticates a plain HTTPS GET.
// This avoids `access tcp`, which can't bridge a raw TCP stream onto an HTTP
// origin (the WebSocket handshake fails -> the browser sees a connection reset).
func (t *tunnel) startWeb(a *App) error {
	t.lc.Lock()
	defer t.lc.Unlock()
	if t.running {
		return nil
	}

	t.setStatus(a, StatusStarting, "Starting map proxy…")
	t.setLastErr("")

	host := t.Hostname
	id, secret := t.ServiceTokenID, t.ServiceTokenSecret
	localURL := t.webURL()

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "https", Host: host})
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		req.Host = host // Cloudflare routes on Host and matches the Access app by it
		if id != "" && secret != "" {
			req.Header.Set("CF-Access-Client-Id", id)
			req.Header.Set("CF-Access-Client-Secret", secret)
		}
	}
	// Keep the browser on the branded local URL: rewrite any redirect that points
	// back at the public (login-walled) hostname.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Header.Set("Location", strings.Replace(loc, "https://"+host, localURL, 1))
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		t.setLastErr(err.Error())
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintln(w, "map proxy: could not reach the map -", err.Error())
	}

	ln, err := net.Listen("tcp", t.localAddr())
	if err != nil {
		t.setStatus(a, StatusError, "failed to bind "+t.localAddr()+": "+err.Error())
		return err
	}

	srv := &http.Server{Handler: proxy}
	t.websrv = srv
	t.running = true
	t.stopping = false

	go func() {
		serveErr := srv.Serve(ln)
		t.lc.Lock()
		t.running = false
		stopping := t.stopping
		t.lc.Unlock()
		if stopping || errors.Is(serveErr, http.ErrServerClosed) {
			t.setStatus(a, StatusStopped, "Disconnected")
		} else {
			t.setStatus(a, StatusError, "map proxy stopped: "+serveErr.Error())
		}
	}()

	if a.log != nil {
		a.log.line(t.Label, "system", "map proxy listening on "+t.localAddr()+" -> https://"+host)
	}
	t.setStatus(a, StatusConnected, "Connected")
	return nil
}

// stop tears the process down. It signals "we asked for this" so the subsequent
// exit is reported as a clean disconnect rather than an error.
func (t *tunnel) stop(a *App) {
	t.lc.Lock()
	if !t.running {
		t.lc.Unlock()
		return
	}
	t.stopping = true
	// Web targets run an in-process HTTP server, not a child process.
	if t.Web {
		srv := t.websrv
		t.lc.Unlock()
		if srv != nil {
			_ = srv.Close()
		}
		return
	}
	cmd, ctrl, cancel := t.cmd, t.ctrl, t.cancel
	t.lc.Unlock()

	// ctrl.kill terminates the whole process group / job (no orphans); cancel()
	// is a belt-and-suspenders fallback through the exec context.
	if ctrl != nil && cmd != nil {
		_ = ctrl.kill(cmd)
	}
	if cancel != nil {
		cancel()
	}
}

// wait reaps the child and reports the terminal status.
func (t *tunnel) wait(a *App) {
	err := t.cmd.Wait()

	t.lc.Lock()
	t.running = false
	stopping := t.stopping
	t.lc.Unlock()

	switch {
	case stopping:
		t.setStatus(a, StatusStopped, "Disconnected")
	case err != nil:
		msg := "cloudflared exited unexpectedly"
		if le := t.getLastErr(); le != "" {
			msg += ": " + le
		} else {
			msg += " (" + err.Error() + ")"
		}
		t.setStatus(a, StatusError, msg)
	default:
		t.setStatus(a, StatusStopped, "cloudflared exited")
	}
}

// readPipe streams a child output stream to the UI log and inspects each line
// for state transitions (auth prompts, "listening", errors).
func (t *tunnel) readPipe(a *App, r io.Reader, stream string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	for sc.Scan() {
		line := sc.Text()
		a.log.line(t.Label, stream, line)
		t.inspectLine(a, line)
	}
}

// inspectLine drives the per-target state machine off cloudflared's logging:
// a login URL means we need browser auth; a listener line means connected.
func (t *tunnel) inspectLine(a *App, line string) {
	low := strings.ToLower(line)

	if url := urlRegexp.FindString(line); url != "" && looksLikeURL(url) {
		isAuth := strings.Contains(url, "cloudflareaccess.com") ||
			strings.Contains(low, "browser") ||
			strings.Contains(low, "log in") ||
			strings.Contains(low, "login") ||
			strings.Contains(low, "authorize") ||
			strings.Contains(low, "authenticate") ||
			strings.Contains(low, "open the following")
		if isAuth {
			t.setStatus(a, StatusWaitingAuth, "Waiting for browser authentication…")
			runtime.EventsEmit(a.ctx, "auth", map[string]string{"id": t.ID, "url": url})
			runtime.BrowserOpenURL(a.ctx, url)
			return
		}
	}

	// cloudflared prints something like "Start Websocket listener on: 127.0.0.1:PORT"
	// once the local proxy is up and ready to accept the game client. That only
	// means the tunnel itself is up, not that the real Minecraft server behind it
	// is answering - so check before claiming "Connected" instead of assuming it.
	if strings.Contains(low, "start websocket listener") ||
		strings.Contains(low, "start serving") ||
		strings.Contains(low, "listening on") {
		t.setStatus(a, StatusChecking, "Checking connection…")
		go func() {
			// A local Minecraft server answers almost instantly, which would flip
			// straight to "Connected" before the user ever registers "Checking
			// connection…" happened. Hold the state for a floor duration so the
			// check is actually visible; this only delays the UI update, not the
			// ping itself, and pingOnce takes at least as long as pingTimeout when
			// the server doesn't answer at all.
			time.Sleep(checkConnectionFloor)
			a.pingOnce(t)
		}()
		return
	}

	// The local listener can be up while the actual tunnel fails when a real
	// connection arrives (hostname not published, auth denied, server down).
	// Surface that instead of leaving a misleading "Connected".
	if strings.Contains(low, "failed to connect to origin") ||
		strings.Contains(low, "unable to connect to origin") {
		reason := "the server could not be reached"
		if i := strings.Index(line, `error="`); i >= 0 {
			rest := line[i+len(`error="`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				reason = rest[:j]
			}
		}
		lowReason := strings.ToLower(reason)

		// A missing DNS record is a real, permanent misconfiguration - report it.
		if strings.Contains(lowReason, "no such host") {
			t.setStatus(a, StatusError,
				"Can't reach server: "+t.Hostname+" is not published on the tunnel (no DNS record)")
			return
		}

		// Transient edge/handshake hiccups (e.g. "websocket: bad handshake",
		// resets, timeouts, EOF) happen on individual connections and clear on
		// retry: cloudflared keeps the local listener up and the player reconnects
		// fine. Don't flip a working tunnel to a sticky error over one of these -
		// just remember it for exit logging and let the live ping (pingLoop) be the
		// judge of whether the server is actually reachable.
		if isTransientOriginErr(lowReason) {
			t.setLastErr(strings.TrimSpace(line))
			return
		}

		// A hard origin failure (connection refused, no route): the tunnel itself
		// is fine, the Minecraft server behind it just isn't answering. The ping
		// loop will clear this back to Connected as soon as the server answers again.
		t.setStatus(a, StatusUnreachable, "Unreachable")
		return
	}

	// Remember the most recent error-ish line so an unexpected exit can quote it.
	if strings.Contains(low, "error") || strings.Contains(low, "failed") ||
		strings.Contains(low, "unable") || strings.Contains(low, "refused") {
		t.setLastErr(strings.TrimSpace(line))
	}
}

// ----- status helpers (st mutex) --------------------------------------------

func (t *tunnel) setStatus(a *App, status, message string) {
	t.st.Lock()
	t.status = status
	t.message = message
	discarded := t.discarded
	t.st.Unlock()
	// A discarded tunnel (removed by a live config change) must not emit UI events:
	// its card is already gone, and a late "stopped" from stop() would recreate it
	// as a ghost "Not connected" row. We still log the transition below.
	if !discarded && a != nil && a.ctx != nil {
		runtime.EventsEmit(a.ctx, "status", t.state())
	}
	// Mirror notable transitions to the file/Discord log as a rich embed (skip
	// the noisy transient "starting" state).
	if a != nil && a.log != nil {
		switch status {
		case StatusConnected, StatusWaitingAuth, StatusUnreachable, StatusError, StatusStopped:
			a.log.statusEmbed(t.Label, t.displayAddr(), status, message)
		}
	}
}

func (t *tunnel) currentStatus() string { t.st.Lock(); defer t.st.Unlock(); return t.status }

// markDiscarded flags a tunnel as removed by a live config change, so any status
// change from its (async) teardown is swallowed rather than re-rendering its card.
func (t *tunnel) markDiscarded() { t.st.Lock(); t.discarded = true; t.st.Unlock() }

// isRunning reports whether the cloudflared child is still up (its local listener
// is live), so a ping to it is worth attempting.
func (t *tunnel) isRunning() bool { t.lc.Lock(); defer t.lc.Unlock(); return t.running }

// isTransientOriginErr reports whether a cloudflared "failed to connect to
// origin" reason is a per-connection hiccup that clears on retry, rather than a
// persistent failure worth surfacing as an error. lowReason must be lower-cased.
func isTransientOriginErr(lowReason string) bool {
	for _, s := range []string{
		"bad handshake",
		"connection reset",
		"reset by peer",
		"broken pipe",
		"timeout",
		"timed out",
		"i/o timeout",
		"eof",
		"temporarily unavailable",
	} {
		if strings.Contains(lowReason, s) {
			return true
		}
	}
	return false
}

func (t *tunnel) setLastErr(s string) { t.st.Lock(); t.lastErr = s; t.st.Unlock() }
func (t *tunnel) getLastErr() string  { t.st.Lock(); defer t.st.Unlock(); return t.lastErr }

func (t *tunnel) state() TargetState {
	t.st.Lock()
	status, message := t.status, t.message
	t.st.Unlock()
	webURL := ""
	if t.Web {
		webURL = t.webURL()
	}
	return TargetState{
		ID:        t.ID,
		Label:     t.Label,
		Hostname:  t.Hostname,
		Protocol:  t.Protocol,
		LocalPort: t.BindPort,
		LocalAddr: t.displayAddr(),
		Status:    status,
		Message:   message,
		Web:       t.Web,
		WebURL:    webURL,
		CoupledTo: t.CoupledTo,
	}
}

// ----- App-level helpers -----------------------------------------------------

func (a *App) binaryPath() string { return filepath.Join(a.dataDir, binaryName()) }

func (a *App) emitBinary(state string, progress int, message string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "binary", map[string]interface{}{
		"state":    state,
		"progress": progress,
		"message":  message,
	})
}

func (a *App) emitHostsError(message string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "hostsError", map[string]string{"message": message})
}

func (a *App) setBinaryStatus(ready bool, message string) {
	a.binaryReady = ready
	a.binaryStatus = message
	state := "downloading"
	if ready {
		state = "ready"
	}
	a.emitBinary(state, 100, message)
}
