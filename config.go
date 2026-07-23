package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errCodeRevoked is returned by fetchRemoteConfig when the endpoint reports the
// code is unknown (HTTP 404) - i.e. the user's access was revoked (or the code
// was mistyped). The live config-refresh loop uses this to send the user back to
// the access-code entry screen.
var errCodeRevoked = errors.New("this access code is no longer valid")

// Target describes one Cloudflare Access route the launcher can open.
type Target struct {
	// Label is the human-friendly name shown in the UI (e.g. "ChromaCube").
	Label string `json:"label"`
	// Hostname is the public Cloudflare hostname the tunnel is published on, i.e.
	// what `cloudflared access` connects out to (e.g. "chromacube1.deforce.site").
	Hostname string `json:"hostname"`
	// Protocol is "tcp" or "udp" and maps to `cloudflared access tcp|udp`.
	Protocol string `json:"protocol"`

	// McHost (hostname mode): the address the player types in Minecraft, e.g.
	// "chromacube.deforce.site". When set, the launcher maps this name to a
	// dedicated loopback IP in the OS hosts file and binds cloudflared there on
	// Minecraft's default port (25565), so the player needs no ":port" suffix.
	McHost string `json:"mcHost"`

	// LocalPort (localhost mode): only used when McHost is empty. cloudflared
	// binds 127.0.0.1:<LocalPort> and the player types "localhost:<LocalPort>".
	LocalPort int `json:"localPort"`

	// Web marks an HTTP target that is opened in a browser (e.g. the live map)
	// instead of joined in Minecraft. It is served exactly like a hostname-mode
	// target - a branded local name (McHost) mapped through the hosts file to a
	// loopback IP where the authenticated cloudflared proxy listens - but on
	// WebPort and with no Server List Ping. The launcher shows an "Open Map"
	// button that points a browser at http://<McHost>.
	Web bool `json:"web,omitempty"`
	// WebPort is the local port a web target binds. Defaults to defaultWebPort
	// (80 on Windows, so the branded hostname needs no ":port" suffix; a high
	// port elsewhere, since binding 80 needs root there). Ignored for non-web
	// targets.
	WebPort int `json:"webPort,omitempty"`
	// CoupledTo (web targets only) is the Hostname of the Minecraft target this web
	// target belongs to (e.g. the map is coupled to "chromacube.deforce.site"). The
	// UI hides the web target's own card and instead shows an "Open Map" button on
	// the coupled server's card. Empty means it renders as its own standalone card.
	CoupledTo string `json:"coupledTo,omitempty"`

	// ServiceTokenID/Secret authenticate this route to Cloudflare Access without
	// a browser login (headless). If empty, the top-level token (if any) is used;
	// if that is also empty the route falls back to interactive browser auth.
	// These are passed to cloudflared via env vars, never on the command line.
	ServiceTokenID     string `json:"serviceTokenId"`
	ServiceTokenSecret string `json:"serviceTokenSecret"`
}

// Config is the launcher configuration, whether loaded from disk or fetched from
// the remote endpoint. The remote (Worker) response uses this same shape.
type Config struct {
	// DisplayName identifies this user/group in the Discord embeds.
	DisplayName string `json:"displayName"`
	// DiscordWebhook receives the session logs (the app has no in-app log panel).
	DiscordWebhook string `json:"discordWebhook"`
	// ServiceTokenID/Secret are the default Access service token applied to every
	// target that does not specify its own.
	ServiceTokenID     string   `json:"serviceTokenId"`
	ServiceTokenSecret string   `json:"serviceTokenSecret"`
	Targets            []Target `json:"targets"`
}

// loadConfig resolves the configuration with the following precedence:
//  1. config.json sitting next to the executable (manual/dev override).
//  2. if this build carries a buildCode: the remote endpoint, falling back to
//     the last successfully cached remote config when offline.
//  3. the embedded default config.json (developer builds with no buildCode).
//
// We deliberately do NOT cache the *embedded* config in the app data dir: an
// auto-written copy would shadow future updates. The remote config IS cached,
// but only as an offline fallback for the exact same buildCode.
func loadConfig(dataDir string, defaultJSON []byte, code string) (Config, error) {
	if exe, err := os.Executable(); err == nil {
		path := filepath.Join(filepath.Dir(exe), "config.json")
		if data, rerr := os.ReadFile(path); rerr == nil {
			cfg, perr := parseConfig(data)
			if perr != nil {
				return Config{}, fmt.Errorf("%s: %w", path, perr)
			}
			return cfg, nil
		}
	}

	if code != "" {
		cfg, err := fetchRemoteConfig(dataDir, code)
		if err == nil {
			return cfg, nil
		}
		if cached, cerr := loadCachedRemote(dataDir); cerr == nil {
			return cached, nil
		}
		return Config{}, fmt.Errorf("could not fetch the server list for this launcher (and no cached copy is available): %w", err)
	}

	cfg, err := parseConfig(defaultJSON)
	if err != nil {
		return Config{}, fmt.Errorf("embedded config.json: %w", err)
	}
	return cfg, nil
}

// exeAdjacentConfigExists reports whether a config.json sits next to the binary
// (a manual/dev override that should take precedence over prompting for a code).
func exeAdjacentConfigExists() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(exe), "config.json"))
	return err == nil
}

func remoteCachePath(dataDir string) string {
	return filepath.Join(dataDir, "remote-config.json")
}

// fetchRemoteConfig asks the Worker for this build's config and, on success,
// caches the raw response for offline use.
func fetchRemoteConfig(dataDir, code string) (Config, error) {
	endpoint := remoteConfigURL + "?code=" + url.QueryEscape(code)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return Config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Config{}, errCodeRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("remote config returned HTTP %d", resp.StatusCode)
	}
	data := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		data = append(data, buf[:n]...)
		if rerr != nil {
			break
		}
		if len(data) > 1<<20 { // 1 MiB sanity cap
			break
		}
	}
	cfg, perr := parseConfig(data)
	if perr != nil {
		return Config{}, fmt.Errorf("remote config invalid: %w", perr)
	}
	_ = os.WriteFile(remoteCachePath(dataDir), data, 0o600) // 0600: may contain a token
	return cfg, nil
}

func loadCachedRemote(dataDir string) (Config, error) {
	data, err := os.ReadFile(remoteCachePath(dataDir))
	if err != nil {
		return Config{}, err
	}
	return parseConfig(data)
}

// firstLabel returns the first DNS label of a hostname (used in error hints).
func firstLabel(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

func parseConfig(data []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if len(cfg.Targets) == 0 {
		return Config{}, fmt.Errorf("no targets defined")
	}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.Protocol == "" {
			t.Protocol = "tcp"
		}
		if t.Protocol != "tcp" && t.Protocol != "udp" {
			return Config{}, fmt.Errorf("target %q: protocol must be tcp or udp, got %q", t.Label, t.Protocol)
		}
		if t.Hostname == "" {
			return Config{}, fmt.Errorf("target %q: hostname is required", t.Label)
		}
		// Web targets are served in hostname mode (branded local name) on WebPort,
		// which defaults to defaultWebPort (80 on Windows, so the browser needs no
		// ":port"; a high port elsewhere, since binding 80 there needs root).
		if t.Web && t.WebPort == 0 {
			t.WebPort = defaultWebPort
		}
		// Either hostname mode (McHost) or localhost mode (LocalPort) must be valid.
		if t.McHost == "" {
			if t.LocalPort <= 0 || t.LocalPort > 65535 {
				return Config{}, fmt.Errorf("target %q: set mcHost, or a valid localPort (1-65535)", t.Label)
			}
		}
		// The player-facing name must differ from the tunnel origin hostname,
		// otherwise the hosts-file redirect would send cloudflared's own origin
		// lookup to loopback and the connection would abort. See README.
		if t.McHost != "" && t.McHost == t.Hostname {
			return Config{}, fmt.Errorf("target %q: mcHost (%q) must differ from hostname (%q); use a short local name like %q",
				t.Label, t.McHost, t.Hostname, firstLabel(t.Hostname))
		}
		if t.Label == "" {
			t.Label = t.Hostname
		}
		// Inherit the top-level service token when the target has none of its own.
		if t.ServiceTokenID == "" {
			t.ServiceTokenID = cfg.ServiceTokenID
		}
		if t.ServiceTokenSecret == "" {
			t.ServiceTokenSecret = cfg.ServiceTokenSecret
		}
	}
	return cfg, nil
}
