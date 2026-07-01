package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	StatusConnected   = "connected"
	StatusError       = "error"
	StatusStopped     = "stopped"
)

// urlRegexp extracts the first http(s) URL from a cloudflared log line.
var urlRegexp = regexp.MustCompile(`https?://[^\s"'<>]+`)

// App is the single Wails-bound object. It owns the cloudflared binary, the
// configured tunnels and all child-process lifecycle.
type App struct {
	ctx        context.Context
	dataDir    string
	defaultCfg []byte

	cfg   Config
	order []*tunnel          // config order, for stable UI rendering
	byID  map[string]*tunnel // id -> tunnel

	needsCode   bool   // universal build awaiting a personal access code
	reallyQuit  bool   // true once the user chose Close from the tray menu
	displayName string // who this user/group is (from remote config)
	installID   string // stable per-install id, for log correlation

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

	// st guards the observable status fields below.
	st      sync.Mutex
	status  string
	message string
	lastErr string
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
}

// AppState is the full snapshot the UI requests on load.
type AppState struct {
	BinaryReady  bool          `json:"binaryReady"`
	BinaryStatus string        `json:"binaryStatus"`
	NeedsCode    bool          `json:"needsCode"`    // show the access-code entry screen
	CodeMode     bool          `json:"codeMode"`     // build identifies users by typed code
	DisplayName  string        `json:"displayName"`  // current user/group label
	Targets      []TargetState `json:"targets"`

	UpdateRequired bool   `json:"updateRequired"` // lock the UI behind the update screen
	UpdateURL      string `json:"updateURL"`      // download link for the new build
	LatestVersion  string `json:"latestVersion"`  // latest published version
	AppVersion     string `json:"appVersion"`     // this build's version
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
	a.cfg = cfg
	a.displayName = displayNameFor(cfg.DisplayName)
	a.log = newLogger(a.dataDir, cfg.DiscordWebhook, buildSessionInfo(a.displayName, a.installID))

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
		if tgt.McHost != "" {
			// Hostname mode: give each target its own loopback IP (127.0.0.1,
			// 127.0.0.2, ...) and bind on Minecraft's default port so the player
			// types a bare hostname. The hosts file maps McHost -> this IP.
			t.HostnameMode = true
			t.BindIP = fmt.Sprintf("127.0.0.%d", loopIdx+1)
			t.BindPort = mcDefaultPort
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

	// If we were just relaunched by a self-update, show what changed.
	if a.justUpdated {
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
	if !a.needsCode {
		a.installHosts()
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
			runtime.WindowShow(a.ctx)
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

// pingLoop measures latency to each connected server every few seconds and
// emits "ping" events ({id, ms}; ms = -1 means unreachable).
func (a *App) pingLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, t := range a.order {
			if t.currentStatus() != StatusConnected {
				continue
			}
			go func(t *tunnel) {
				ms, err := pingMinecraft(t.localAddr())
				payload := map[string]interface{}{"id": t.ID, "ms": -1}
				if err == nil {
					payload["ms"] = ms
				}
				if a.ctx != nil {
					runtime.EventsEmit(a.ctx, "ping", payload)
				}
			}(t)
		}
	}
}

// showWindow brings the hidden (tray) window back to the foreground and re-checks
// for a new version (so reopening from the tray behaves like a fresh check).
func (a *App) showWindow() {
	if a.ctx != nil {
		runtime.WindowShow(a.ctx)
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

// announceUpdate shows the "what changed" popup after a self-update, using the
// latest GitHub release notes.
func (a *App) announceUpdate() {
	tag, body := fetchLatestRelease()
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "updated", map[string]interface{}{
		"version": appVersion,
		"tag":     tag,
		"notes":   body,
	})
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

// beforeClose runs on the UI thread when the window's close button is pressed.
// We hide to the tray and keep running, unless the user explicitly chose "Close"
// from the tray menu (reallyQuit), in which case we let the close proceed.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.reallyQuit {
		return false
	}
	runtime.WindowHide(ctx)
	go func() {
		if err := notify("ChromaCube Launcher", "Still running in the tray. Right-click the tray icon and choose Close to quit."); err != nil {
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
	// the outgoing process must not strip it out from under the new one.
	if !a.skipHostsCleanup {
		a.removeHosts()
	}
	a.log.sessionEnd()
	a.log.close()
}

// serverLabels lists the configured server names for the session-start embed.
func (a *App) serverLabels() []string {
	labels := make([]string, 0, len(a.order))
	for _, t := range a.order {
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

	targets := make([]TargetState, 0, len(a.order))
	for _, t := range a.order {
		targets = append(targets, t.state())
	}
	return AppState{
		BinaryReady:  ready,
		BinaryStatus: bs,
		NeedsCode:    a.needsCode,
		CodeMode:     codeMode(),
		DisplayName:  a.displayName,
		Targets:      targets,

		UpdateRequired: a.updateRequired,
		UpdateURL:      a.updateURL,
		LatestVersion:  a.latestVersion,
		AppVersion:     appVersion,
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
	a.needsCode = false
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
	a.DisconnectAll()
	a.removeHosts()
	clearAccessCode(a.dataDir)
	if a.log != nil {
		a.log.close()
	}

	a.needsCode = true
	a.cfg = Config{}
	a.order = nil
	a.byID = map[string]*tunnel{}
	a.hostsEntries = nil
	a.hostnameMode = false
	a.displayName = displayNameFor("")
	a.log = newLogger(a.dataDir, "", buildSessionInfo(a.displayName, a.installID))
	a.log.start()

	runtime.EventsEmit(a.ctx, "reloaded", a.GetState())
}

// Connect starts the tunnel for a single target id.
func (a *App) Connect(id string) error {
	if err := a.ensureBinary(); err != nil {
		return fmt.Errorf("cloudflared not available: %w", err)
	}
	t := a.byID[id]
	if t == nil {
		return fmt.Errorf("unknown target %q", id)
	}
	return t.start(a)
}

// Disconnect stops the tunnel for a single target id.
func (a *App) Disconnect(id string) error {
	t := a.byID[id]
	if t == nil {
		return fmt.Errorf("unknown target %q", id)
	}
	t.stop(a)
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
	for _, t := range a.order {
		_ = t.start(a)
	}
}

// DisconnectAll stops every configured tunnel. Safe to call multiple times.
func (a *App) DisconnectAll() {
	for _, t := range a.order {
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

// start launches `cloudflared access <tcp|udp> --hostname H --url 127.0.0.1:PORT`.
// It is idempotent: a second call while running is a no-op.
func (t *tunnel) start(a *App) error {
	// Guard against the synthetic config-error placeholder (no real target).
	if t.Hostname == "" || t.Protocol == "" {
		return nil
	}

	t.lc.Lock()
	defer t.lc.Unlock()
	if t.running {
		return nil
	}

	t.setStatus(a, StatusStarting, "Starting cloudflared…")
	t.setLastErr("")

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

// stop tears the process down. It signals "we asked for this" so the subsequent
// exit is reported as a clean disconnect rather than an error.
func (t *tunnel) stop(a *App) {
	t.lc.Lock()
	if !t.running {
		t.lc.Unlock()
		return
	}
	t.stopping = true
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
	// once the local proxy is up and ready to accept the game client.
	if strings.Contains(low, "start websocket listener") ||
		strings.Contains(low, "start serving") ||
		strings.Contains(low, "listening on") {
		t.setStatus(a, StatusConnected, "Connected")
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
		if strings.Contains(strings.ToLower(reason), "no such host") {
			reason = t.Hostname + " is not published on the tunnel (no DNS record)"
		}
		t.setStatus(a, StatusError, "Can't reach server: "+reason)
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
	t.st.Unlock()
	if a != nil && a.ctx != nil {
		runtime.EventsEmit(a.ctx, "status", t.state())
	}
	// Mirror notable transitions to the file/Discord log as a rich embed (skip
	// the noisy transient "starting" state).
	if a != nil && a.log != nil {
		switch status {
		case StatusConnected, StatusWaitingAuth, StatusError, StatusStopped:
			a.log.statusEmbed(t.Label, t.displayAddr(), status, message)
		}
	}
}

func (t *tunnel) currentStatus() string { t.st.Lock(); defer t.st.Unlock(); return t.status }

func (t *tunnel) setLastErr(s string) { t.st.Lock(); t.lastErr = s; t.st.Unlock() }
func (t *tunnel) getLastErr() string  { t.st.Lock(); defer t.st.Unlock(); return t.lastErr }

func (t *tunnel) state() TargetState {
	t.st.Lock()
	status, message := t.status, t.message
	t.st.Unlock()
	return TargetState{
		ID:        t.ID,
		Label:     t.Label,
		Hostname:  t.Hostname,
		Protocol:  t.Protocol,
		LocalPort: t.BindPort,
		LocalAddr: t.displayAddr(),
		Status:    status,
		Message:   message,
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
