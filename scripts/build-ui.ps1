param([switch]$SkipInstall)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
Push-Location (Join-Path $projectRoot 'web')
try {
  if (-not $SkipInstall) { npm.cmd ci; if ($LASTEXITCODE -ne 0) { throw 'Web dependency installation failed' } }
  npm.cmd run build
  if ($LASTEXITCODE -ne 0) { throw 'Web build failed' }
  npm.cmd test
  if ($LASTEXITCODE -ne 0) { throw 'Web tests failed' }
  $sourceDirectory = [IO.Path]::GetFullPath((Join-Path $projectRoot 'web/dist'))
  $assetDirectory = [IO.Path]::GetFullPath((Join-Path $projectRoot 'internal/ui/assets'))
  $expectedAssetDirectory = [IO.Path]::GetFullPath((Join-Path $projectRoot 'internal/ui/assets'))
  $rootPrefix = [IO.Path]::GetFullPath($projectRoot).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
  if ($assetDirectory -ne $expectedAssetDirectory -or -not $assetDirectory.StartsWith($rootPrefix,[StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing to replace assets outside the project.' }
  if (-not (Test-Path -LiteralPath (Join-Path $sourceDirectory 'index.html') -PathType Leaf)) { throw 'Web production entry point missing.' }
  if (Test-Path -LiteralPath $assetDirectory) {
    if ((Get-Item -LiteralPath $assetDirectory).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Refusing to replace an asset-directory link.' }
    Remove-Item -LiteralPath $assetDirectory -Recurse -Force
  }
  New-Item -ItemType Directory -Path $assetDirectory | Out-Null
  Get-ChildItem -LiteralPath $sourceDirectory | ForEach-Object { Copy-Item -LiteralPath $_.FullName -Destination $assetDirectory -Recurse }
} finally { Pop-Location }
