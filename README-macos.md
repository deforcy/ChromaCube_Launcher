# ChromaCube Launcher — macOS build

This `macos/` folder is a copy of the launcher with the macOS-specific changes
needed to build and run it as a native `.app`. One **universal** build runs on
both target audiences:

- **Newer Macs (Apple Silicon — M1/M2/M3/M4)** → `arm64`
- **Older Intel Macs** → `amd64` (`x86_64`)

## ⚠️ You must build on a Mac

Wails renders the UI through macOS's Cocoa/WebKit frameworks via **cgo**. That
means the macOS app **cannot be cross-compiled from Windows or Linux** — there
is no way to produce the working `.app` on the Windows machine this project
normally builds on. Copy this `macos/` folder to a Mac and build it there.

(The Go code in here was verified to compile for `darwin/arm64` and
`darwin/amd64`; only the final Wails link step needs the Mac toolchain.)

## Prerequisites (on the Mac)

```sh
xcode-select --install                                             # C toolchain
# Go 1.22+ from https://go.dev/dl/
go install github.com/wailsapp/wails/v2/cmd/wails@latest           # Wails CLI
```

## Build

```sh
chmod +x build-macos.sh        # first time only

./build-macos.sh               # universal .app (Intel + Apple Silicon) — recommended
./build-macos.sh intel         # Intel-only (amd64), smaller, older Macs
./build-macos.sh applesilicon  # Apple Silicon-only (arm64), smaller, newer Macs
```

Output: `build/bin/ChromaCube Launcher.app`. Confirm it is universal with:

```sh
lipo -archs "build/bin/ChromaCube Launcher.app/Contents/MacOS/"*
# -> x86_64 arm64
```

Baking a group code or overriding the config endpoint works the same as the
Windows scripts, via environment variables:

```sh
VERSION=1.2.0 CODE=sv-7h3k ./build-macos.sh          # per-group build, no code prompt
VERSION=1.2.0 REMOTE_URL=https://api.deforce.site/ ./build-macos.sh   # universal build
```

## First launch (Gatekeeper)

The script **ad-hoc signs** the app so macOS won't call it "damaged", but that
is not an Apple Developer ID signature. On first launch, users either:

- right-click the app → **Open** → **Open**, or
- run `xattr -dr com.apple.quarantine "/Applications/ChromaCube Launcher.app"`.

For frictionless distribution, sign with a Developer ID certificate and the
Hardened Runtime (entitlements are provided in `build/darwin/entitlements.plist`)
and notarize the app.

## What changed for macOS (vs. the Windows build)

| Area | Windows | macOS (this build) |
|---|---|---|
| `/etc/hosts` editing | Forced admin at launch via exe manifest | Prompts for admin **only** when the hosts block changes (`osascript` → Touch ID / password) — see `hosts_darwin.go` |
| Autostart at login | Logon scheduled task | Per-user **LaunchAgent** in `~/Library/LaunchAgents` — see `autostart_darwin.go` |
| Notifications | Windows toast | `osascript display notification` — see `notify_darwin.go` |
| System tray | Tray icon + menu | **No tray/menu-bar icon** in this build (the tray lib conflicts with the Wails main thread on macOS). The app keeps its Dock icon; click it to re-open the window. |
| cloudflared binary | `cloudflared-windows-*.exe` | `cloudflared-darwin-{amd64,arm64}.tgz`, auto-downloaded & extracted (already handled in `cloudflared.go`) |

### The admin prompt

Hostname mode redirects player-facing hostnames in `/etc/hosts`, which requires
root. macOS apps can't force elevation at launch the way the Windows manifest
does, so the launcher asks for administrator rights **at the moment it changes
the hosts file** (typically once on connect and once on disconnect). If the user
cancels the prompt, hostname mode simply can't apply and the existing in-app
error is shown — everything else still works.

## Minimum macOS version

`LSMinimumSystemVersion` is **10.13 (High Sierra)** in `build/darwin/Info.plist`,
which is the floor Wails/WebKit support and comfortably covers the older Intel
Macs this build targets.
