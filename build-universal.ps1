# Build the ONE universal launcher .exe for per-user distribution.
#
#   .\build-universal.ps1
#   .\build-universal.ps1 -Version 1.2.0 -RemoteURL "https://launcher-config.YOU.workers.dev/"
#
# This build has no baked-in code: on first launch it asks each user for their
# personal access code, then fetches that user's servers from the remote endpoint.
# Manage users in the Worker/KV (and the admin panel) - you never rebuild per user.

param(
  [string]$Name = "ChromaCube-Launcher",
  [string]$Version = "1.4.3",
  [string]$RemoteURL = ""
)

$env:PATH = "C:\Program Files\Go\bin;$env:USERPROFILE\go\bin;$env:PATH"

# requireUserCode=true turns on the first-launch code prompt; buildCode stays empty.
$ld = "-X main.requireUserCode=true -X main.appVersion=$Version"
if ($RemoteURL -ne "") {
  $ld = "$ld -X main.remoteConfigURL=$RemoteURL"
}

Write-Host "Building universal '$Name.exe' (v$Version)..." -ForegroundColor Cyan
wails build -platform windows/amd64 -ldflags $ld -o "$Name.exe"

if ($LASTEXITCODE -eq 0) {
  Write-Host "Done: build\bin\$Name.exe" -ForegroundColor Green
} else {
  Write-Host "Build failed (exit $LASTEXITCODE)" -ForegroundColor Red
}
