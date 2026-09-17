param([string]$Python = 'python', [switch]$SkipInstall)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$dependencies = Join-Path $projectRoot '.tools/provision-build-deps'
$work = Join-Path $projectRoot '.tools/provision-build'
$output = Join-Path $projectRoot 'dist/tools'
if (-not $SkipInstall) {
  & $Python -m pip install --target $dependencies --requirement (Join-Path $projectRoot 'packaging/offline-tools-requirements.txt')
  if ($LASTEXITCODE -ne 0) { throw 'Offline tool build dependencies failed.' }
}
$previousPythonPath = $env:PYTHONPATH
try {
  $env:PYTHONPATH = $dependencies
  & $Python -S -m PyInstaller --noconfirm --clean --onefile --console --name provision-device --distpath $output --workpath (Join-Path $work 'provision') --specpath $work --collect-all esp_idf_nvs_partition_gen (Join-Path $projectRoot 'scripts/provision-device.py')
  if ($LASTEXITCODE -ne 0) { throw 'Standalone provisioning tool build failed.' }
  & $Python -S -m PyInstaller --noconfirm --clean --onefile --console --name esptool --distpath $output --workpath (Join-Path $work 'esptool') --specpath $work --collect-all esptool (Join-Path $projectRoot 'packaging/esptool_entry.py')
  if ($LASTEXITCODE -ne 0) { throw 'Standalone flashing tool build failed.' }
} finally { $env:PYTHONPATH = $previousPythonPath }
