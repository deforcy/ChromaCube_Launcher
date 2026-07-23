# Build a per-group launcher .exe with an access code baked in.
#
#   .\build-group.ps1 -Code sv-7h3k -Name chromacube-survival
#   .\build-group.ps1 -Code hbm-9x2 -Name chromacube-hbm -Version 1.2.0
#
# The code is injected into main.buildCode at compile time, so the resulting exe
# fetches THAT group's server list from the remote endpoint with no user input.
# A build with no -Code (plain `wails build`) stays a developer build that uses
# the local/embedded config.json.

param(
  [Parameter(Mandatory = $true)][string]$Code,
  [string]$Name = "chromacube-launcher",
  [string]$Version = "1.5.0",
  [string]$RemoteURL = ""
)

$env:PATH = "C:\Program Files\Go\bin;$env:USERPROFILE\go\bin;$env:PATH"

# -X main.<var>=<value> overrides the defaults in buildinfo.go.
$ld = "-X main.buildCode=$Code -X main.appVersion=$Version"
if ($RemoteURL -ne "") {
  $ld = "$ld -X main.remoteConfigURL=$RemoteURL"
}

Write-Host "Building '$Name.exe' for code '$Code' (v$Version)..." -ForegroundColor Cyan
wails build -platform windows/amd64 -ldflags $ld -o "$Name.exe"

if ($LASTEXITCODE -eq 0) {
  Write-Host "Done: build\bin\$Name.exe" -ForegroundColor Green
} else {
  Write-Host "Build failed (exit $LASTEXITCODE)" -ForegroundColor Red
}
