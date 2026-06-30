package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Embed colours (Discord uses a single decimal RGB int).
const (
	colorGreen = 0x38d39f // connected
	colorAmber = 0xf5b95c // connecting / waiting for auth
	colorRed   = 0xff6b6b // error
	colorBlue  = 0x5cc8f5 // session / info
	colorGray  = 0x9aa0b2 // stopped / neutral
)

// sessionInfo is the per-run identity context attached to every embed so the
// owner can tell, at a glance, who connected and from where.
type sessionInfo struct {
	DisplayName string
	InstallID   string
	Version     string
	OS          string
	IP          string
	Location    string
}

func buildSessionInfo(displayName, installID string) sessionInfo {
	return sessionInfo{
		DisplayName: displayName,
		InstallID:   installID,
		Version:     appVersion,
		OS:          runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// logger writes every cloudflared line to a per-session file and forwards
// notable events to a Discord webhook as rich embeds. Discord posts go through a
// single serial queue so messages stay ordered and we never trip the webhook
// rate limit, and so a slow POST never blocks the connection path.
type logger struct {
	webhook string

	mu   sync.Mutex
	file *os.File
	info sessionInfo

	client *http.Client
	queue  chan map[string]interface{} // embed payloads awaiting send
	stop   chan struct{}
	wg     sync.WaitGroup
}

func newLogger(dataDir, webhook string, info sessionInfo) *logger {
	l := &logger{
		webhook: strings.TrimSpace(webhook),
		info:    info,
		client:  &http.Client{Timeout: 15 * time.Second},
		queue:   make(chan map[string]interface{}, 100),
		stop:    make(chan struct{}),
	}
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("session-%s.log", time.Now().Format("2006-01-02_15-04-05"))
	if f, err := os.Create(filepath.Join(dir, name)); err == nil {
		l.file = f
	}
	return l
}

// start launches the serial Discord sender.
func (l *logger) start() {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			select {
			case e := <-l.queue:
				l.post(e)
			case <-l.stop:
				// Drain whatever is queued, then exit.
				for {
					select {
					case e := <-l.queue:
						l.post(e)
					default:
						return
					}
				}
			}
		}
	}()
}

func (l *logger) close() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	l.wg.Wait()
	l.mu.Lock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.mu.Unlock()
}

// line records one raw cloudflared output line to the session file only (raw
// output is not spammed to Discord; the embeds summarise what matters).
func (l *logger) line(label, stream, text string) {
	stamp := time.Now().UTC().Format("15:04:05")
	l.writeFile(fmt.Sprintf("[%s] [%s/%s] %s", stamp, label, stream, text))
}

func (l *logger) writeFile(entry string) {
	l.mu.Lock()
	if l.file != nil {
		_, _ = l.file.WriteString(entry + "\n")
	}
	l.mu.Unlock()
}

// resolveLocation does a best-effort public IP + geo lookup for the embeds.
func (l *logger) resolveLocation() {
	ip, loc := lookupGeo(l.client)
	l.mu.Lock()
	l.info.IP = ip
	l.info.Location = loc
	l.mu.Unlock()
}

func (l *logger) snapshotInfo() sessionInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

// ----- High-level events -----------------------------------------------------

func (l *logger) sessionStart(serverLabels []string) {
	servers := strings.Join(serverLabels, ", ")
	if servers == "" {
		servers = "(none)"
	}
	l.writeFile("=== launcher started ===")
	l.enqueue(l.baseEmbed("Launcher started", colorBlue, [][2]string{
		{"Servers", servers},
	}))
}

func (l *logger) sessionEnd() {
	l.writeFile("=== launcher stopped ===")
	l.enqueue(l.baseEmbed("Launcher stopped", colorGray, nil))
}

// statusEmbed reports a per-server status transition.
func (l *logger) statusEmbed(server, address, status, message string) {
	l.writeFile(fmt.Sprintf("[%s] %s -> %s (%s)", server, address, status, message))

	title, color := server+": ", colorGray
	switch status {
	case StatusConnected:
		title, color = server+": connected", colorGreen
	case StatusWaitingAuth:
		title, color = server+": waiting for sign-in", colorAmber
	case StatusError:
		title, color = server+": error", colorRed
	case StatusStopped:
		title, color = server+": disconnected", colorGray
	}

	fields := [][2]string{{"Server", server}}
	if address != "" {
		fields = append(fields, [2]string{"Address", address})
	}
	if message != "" {
		fields = append(fields, [2]string{"Detail", truncateField(message)})
	}
	l.enqueue(l.baseEmbed(title, color, fields))
}

// ----- Embed construction & sending ------------------------------------------

// baseEmbed builds an embed pre-populated with the session identity fields.
func (l *logger) baseEmbed(title string, color int, extra [][2]string) map[string]interface{} {
	info := l.snapshotInfo()

	fields := []map[string]interface{}{
		{"name": "User", "value": orDash(info.DisplayName), "inline": true},
		{"name": "OS", "value": orDash(info.OS), "inline": true},
		{"name": "Version", "value": orDash(info.Version), "inline": true},
	}
	if info.Location != "" || info.IP != "" {
		loc := info.Location
		if info.IP != "" {
			if loc != "" {
				loc += " (" + info.IP + ")"
			} else {
				loc = info.IP
			}
		}
		fields = append(fields, map[string]interface{}{"name": "Location", "value": loc, "inline": true})
	}
	for _, kv := range extra {
		fields = append(fields, map[string]interface{}{"name": kv[0], "value": orDash(kv[1]), "inline": false})
	}

	return map[string]interface{}{
		"title":     title,
		"color":     color,
		"fields":    fields,
		"footer":    map[string]interface{}{"text": "install " + orDash(info.InstallID)},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
}

func (l *logger) enqueue(embed map[string]interface{}) {
	if l.webhook == "" {
		return
	}
	select {
	case l.queue <- embed:
	default:
		// Queue full: drop rather than block the connection path.
	}
}

// post sends one embed to the webhook. Failures are swallowed: logging must
// never disrupt the user's connection.
func (l *logger) post(embed map[string]interface{}) {
	if l.webhook == "" {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"username": "ChromaCube Launcher",
		"embeds":   []interface{}{embed},
	})
	if err != nil {
		return
	}
	resp, err := l.client.Post(l.webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	// Be gentle with the webhook rate limit between serial sends.
	time.Sleep(400 * time.Millisecond)
}

// ----- Helpers ---------------------------------------------------------------

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func truncateField(s string) string {
	const max = 1000 // Discord field value limit is 1024
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// lookupGeo returns the caller's public IP and a "City, Country" string using a
// free, keyless endpoint. Best effort: any failure yields empty strings.
func lookupGeo(client *http.Client) (ip, location string) {
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var data struct {
		IP      string `json:"ip"`
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", ""
	}
	parts := make([]string, 0, 3)
	for _, p := range []string{data.City, data.Region, data.Country} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return data.IP, strings.Join(parts, ", ")
}
