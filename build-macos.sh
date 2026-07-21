#!/usr/bin/env bash
#
# Build the ChromaCube Launcher .app for macOS.
#
# ┌───────────────────────────────────────────────────────────────────────────┐
# │ MUST be run ON A MAC. Wails links the UI against Cocoa/WebKit through cgo,  │
# │ so it cannot be cross-compiled from Windows or Linux. Copy this macos/      │
# │ folder to a Mac and run it there.                                           │
# └───────────────────────────────────────────────────────────────────────────┘
#
# Requirements on the Mac:
#   - Xcode Command Line Tools ...... xcode-select --install
#   - Go 1.22+ ...................... https://go.dev/dl/
#   - Wails CLI v2.12+ .............. go install github.com/wailsapp/wails/v2/cmd/wails@latest
#
# Usage:
#   ./build-macos.sh                # universal .app: Intel + Apple Silicon  (recommended)
#   ./build-macos.sh universal      # same as above
#   ./build-macos.sh intel          # Intel only (amd64) - for older Intel Macs
#   ./build-macos.sh applesilicon   # Apple Silicon only (arm64) - for newer Macs
#
# One universal build runs on BOTH newer (Apple Silicon) and older (Intel) Macs,
# so it is the simplest thing to ship. The per-arch targets exist only if you
# want a smaller download for one CPU type.
#
# Optional environment variables (mirror the Windows build-*.ps1 scripts):
#   VERSION=1.2.0      version string baked into the binary (default 1.4.3)
#   CODE=sv-7h3k       bake a group access code (like build-group.ps1). If unset,
#                      a UNIVERSAL-config build is produced that prompts each user
#                      for their personal code on first launch (like
#                      build-universal.ps1).
#   REMOTE_URL=https://api.deforce.site/   override the remote config endpoint.
#
set -euo pipefail
cd "$(dirname "$0")"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: this script must run on macOS - Wails macOS builds need the native" >&2
  echo "       Cocoa/WebKit toolchain and cannot be cross-compiled from $(uname -s)." >&2
  exit 1
fi

TARGET="${1:-universal}"
VERSION="${VERSION:-1.4.3}"

case "$TARGET" in
  universal|"")               PLATFORM="darwin/universal" ;;
  intel|amd64|x86_64)         PLATFORM="darwin/amd64" ;;
  applesilicon|apple|arm64|m1) PLATFORM="darwin/arm64" ;;
  *) echo "unknown target '$TARGET' (use: universal | intel | applesilicon)" >&2; exit 1 ;;
esac

# -X main.<var>=<value> overrides the defaults in buildinfo.go (see the .ps1 scripts).
LD="-X main.appVersion=${VERSION}"
if [[ -n "${CODE:-}" ]]; then
  LD="${LD} -X main.buildCode=${CODE}"           # per-group build, no user prompt
else
  LD="${LD} -X main.requireUserCode=true"        # universal build, prompts for code
fi
if [[ -n "${REMOTE_URL:-}" ]]; then
  LD="${LD} -X main.remoteConfigURL=${REMOTE_URL}"
fi

echo "Building ChromaCube Launcher  ->  ${PLATFORM}  (v${VERSION})"
wails build -platform "${PLATFORM}" -ldflags "${LD}" -clean

APP="$(ls -d build/bin/*.app 2>/dev/null | head -1 || true)"
if [[ -z "${APP}" ]]; then
  echo "Build finished but no .app was found under build/bin." >&2
  exit 1
fi

# Ad-hoc sign so Gatekeeper does not report the app as "damaged". This is NOT a
# Developer ID signature: on first launch users still right-click the app and
# choose Open (or run: xattr -dr com.apple.quarantine "<app>"). For real
# distribution, sign with a Developer ID cert + the Hardened Runtime using
# build/darwin/entitlements.plist, then notarize.
echo "Ad-hoc signing ${APP}"
codesign --force --deep --sign - "${APP}" || echo "warning: ad-hoc codesign failed (app may still run)"

echo
echo "Done: ${APP}"
echo "Verify architectures with:  lipo -archs \"${APP}/Contents/MacOS/\"*"
