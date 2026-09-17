#Requires -Version 7.0
param([string]$Version = '0.2.0-preview.2')
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^\d+\.\d+\.\d+([-.][A-Za-z0-9.]+)?$') { throw 'Invalid version' }
$root = Split-Path $PSScriptRoot -Parent
Push-Location $root
$previous = @{}
foreach ($name in @('GOOS','GOARCH','CGO_ENABLED')) { $previous[$name] = [Environment]::GetEnvironmentVariable($name) }
try {
  $html = Get-Content internal/ui/assets/index.html -Raw
  if ($html -notmatch '/assets/[^" ]+\.js') { throw 'Run scripts/build-ui.ps1 first; refusing a placeholder Web build.' }
  $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
  New-Item -ItemType Directory -Force dist/release | Out-Null
  go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$Version" -o dist/release/uart2llm-agent-windows-x64.exe ./cmd/uart2llm
  if ($LASTEXITCODE -ne 0) { throw 'Agent build failed' }
  $notices = Get-ChildItem internal/notices -Filter '*.txt' | Sort-Object Name | ForEach-Object {
    "--- $($_.Name) ---`n" + (Get-Content $_.FullName -Raw)
  }
  $notices -join "`n" | Set-Content dist/release/LICENSES.txt -Encoding utf8NoBOM
  Copy-Item docs/RELEASE.md dist/release/README.md
} finally {
  foreach ($name in $previous.Keys) { [Environment]::SetEnvironmentVariable($name, $previous[$name]) }
  Pop-Location
}
