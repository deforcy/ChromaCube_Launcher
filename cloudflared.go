package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// binaryName returns the platform-specific cloudflared file name.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "cloudflared.exe"
	}
	return "cloudflared"
}

// cloudflaredAssetURL returns the official GitHub "latest release" download URL
// for this OS/arch, and whether that asset is a .tgz archive (true on macOS).
//
// Reference asset names (cloudflare/cloudflared releases):
//   - windows: cloudflared-windows-amd64.exe / cloudflared-windows-386.exe
//   - darwin:  cloudflared-darwin-amd64.tgz / cloudflared-darwin-arm64.tgz
//   - linux:   cloudflared-linux-amd64 / -arm64 / -386 / -arm
func cloudflaredAssetURL() (url string, archived bool, err error) {
	const base = "https://github.com/cloudflare/cloudflared/releases/latest/download/"

	switch runtime.GOOS {
	case "windows":
		arch := "amd64"
		if runtime.GOARCH == "386" {
			arch = "386"
		}
		// Cloudflare does not currently ship a windows-arm64 build; amd64 runs
		// fine under emulation on arm64 Windows.
		return base + "cloudflared-windows-" + arch + ".exe", false, nil

	case "darwin":
		arch := "amd64"
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		}
		return base + "cloudflared-darwin-" + arch + ".tgz", true, nil

	case "linux":
		var arch string
		switch runtime.GOARCH {
		case "amd64":
			arch = "amd64"
		case "arm64":
			arch = "arm64"
		case "386":
			arch = "386"
		case "arm":
			arch = "arm"
		default:
			return "", false, fmt.Errorf("unsupported linux arch %q", runtime.GOARCH)
		}
		return base + "cloudflared-linux-" + arch, false, nil

	default:
		return "", false, fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// ensureBinary makes sure a usable cloudflared exists in the app data dir,
// downloading (and on macOS extracting) it if necessary. It is safe to call
// repeatedly; it is a no-op once the binary is present. Progress is surfaced to
// the UI via "binary" events.
func (a *App) ensureBinary() error {
	a.binMu.Lock()
	defer a.binMu.Unlock()

	dest := a.binaryPath()
	if fileExists(dest) {
		a.setBinaryStatus(true, "cloudflared ready")
		return nil
	}

	url, archived, err := cloudflaredAssetURL()
	if err != nil {
		a.setBinaryStatus(false, "Unsupported platform: "+err.Error())
		return err
	}

	a.emitBinary("downloading", 0, "Downloading cloudflared…")

	resp, err := http.Get(url)
	if err != nil {
		a.setBinaryStatus(false, "Download failed: "+err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("download returned HTTP %d", resp.StatusCode)
		a.setBinaryStatus(false, err.Error())
		return err
	}

	// Stream to a temp file with throttled progress reporting.
	tmp := dest + ".download"
	out, err := os.Create(tmp)
	if err != nil {
		a.setBinaryStatus(false, err.Error())
		return err
	}

	pr := &progressReader{
		reader: resp.Body,
		total:  resp.ContentLength,
		onProgress: func(pct int) {
			a.emitBinary("downloading", pct, "Downloading cloudflared…")
		},
	}
	if _, err := io.Copy(out, pr); err != nil {
		out.Close()
		os.Remove(tmp)
		a.setBinaryStatus(false, "Download failed: "+err.Error())
		return err
	}
	out.Close()

	if archived {
		// macOS ships a gzip-compressed tar containing a single "cloudflared".
		a.emitBinary("downloading", 100, "Extracting cloudflared…")
		if err := extractCloudflaredFromTgz(tmp, dest); err != nil {
			os.Remove(tmp)
			a.setBinaryStatus(false, "Extract failed: "+err.Error())
			return err
		}
		os.Remove(tmp)
	} else {
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			a.setBinaryStatus(false, err.Error())
			return err
		}
	}

	// Make it executable on Unix.
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dest, 0o755)
	}

	a.setBinaryStatus(true, "cloudflared ready")
	return nil
}

// extractCloudflaredFromTgz pulls the "cloudflared" entry out of a .tgz into dest.
func extractCloudflaredFromTgz(tgzPath, dest string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("cloudflared not found inside archive")
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// The binary is named "cloudflared" (possibly nested); match the base name.
		if filepath.Base(hdr.Name) != "cloudflared" {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
}

// progressReader wraps an io.Reader and reports download completion percentage,
// throttled to roughly 5 updates per second so we don't flood the event bus.
type progressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	lastPct    int
	lastEmit   time.Time
	onProgress func(pct int)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.read += int64(n)
	if p.total > 0 && p.onProgress != nil {
		pct := int(float64(p.read) / float64(p.total) * 100)
		if pct != p.lastPct && time.Since(p.lastEmit) > 200*time.Millisecond {
			p.lastPct = pct
			p.lastEmit = time.Now()
			p.onProgress(pct)
		}
	}
	return n, err
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// looksLikeURL is a tiny helper used by log inspection.
func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
