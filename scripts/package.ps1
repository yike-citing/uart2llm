#Requires -Version 7.0
param(
  [string]$Version = '0.2.0-dev',
  [string]$NativeExe = '',
  [string]$MakeNSIS = '',
  [string]$LinuxMakeNSIS = '',
  [string]$LinuxNSISDirectory = '',
  [string]$WSLDistro = 'Ubuntu',
  [switch]$IncludeUSBValidation
)
$ErrorActionPreference = 'Stop'
$projectRoot = [IO.Path]::GetFullPath((Split-Path $PSScriptRoot -Parent))
if ($Version -notmatch '^\d+\.\d+\.\d+([-.][A-Za-z0-9.]+)?$') { throw 'Version must be a safe semantic version.' }
if (-not $NativeExe) { $NativeExe = Join-Path $projectRoot 'native/build/uart2llm-gui.exe' }
$releaseDirectory = Join-Path $projectRoot 'dist'
$stageDirectory = Join-Path $projectRoot ('.tools/package-stage-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $releaseDirectory,$stageDirectory | Out-Null
function Copy-Required([string]$Source, [string]$RelativeDestination) {
  if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) { throw "Missing release input: $Source" }
  $destination = Join-Path $stageDirectory $RelativeDestination
  New-Item -ItemType Directory -Force (Split-Path $destination -Parent) | Out-Null
  Copy-Item -LiteralPath $Source -Destination $destination
}
Copy-Required (Join-Path $projectRoot 'dist/uart2llm.exe') 'uart2llm.exe'
Copy-Required $NativeExe 'uart2llm-gui.exe'
Copy-Required (Join-Path $projectRoot 'README.md') 'README.md'
Copy-Required (Join-Path $projectRoot 'firmware/build/uart2llm.bin') 'firmware/uart2llm.bin'
Copy-Required (Join-Path $projectRoot 'firmware/build/bootloader/bootloader.bin') 'firmware/bootloader/bootloader.bin'
Copy-Required (Join-Path $projectRoot 'firmware/build/partition_table/partition-table.bin') 'firmware/partition_table/partition-table.bin'
Copy-Required (Join-Path $projectRoot 'firmware/build/ota_data_initial.bin') 'firmware/ota_data_initial.bin'
Copy-Required (Join-Path $projectRoot 'firmware/build/flasher_args.json') 'firmware/flasher_args.json'
if ($IncludeUSBValidation) {
  foreach ($name in @('uart2llm.bin','bootloader/bootloader.bin','partition_table/partition-table.bin','ota_data_initial.bin','flasher_args.json')) {
    Copy-Required (Join-Path $projectRoot "firmware/build-usb/$name") "firmware-usb-validation/$name"
  }
  Copy-Required (Join-Path $projectRoot 'firmware/sdkconfig.usb-validation') 'firmware-usb-validation/sdkconfig.defaults'
  Copy-Required (Join-Path $projectRoot 'scripts/hardware-check.py') 'tools/hardware-check.py'
  Copy-Required (Join-Path $projectRoot 'scripts/capture-serial.py') 'tools/capture-serial.py'
  foreach ($name in @('network-check.py','files-check.py','live-recovery-check.py','hardware_check_support.py','opencode-check.py','opencode_check_test.py','tray-check.py','passwordless-check.py','usb-performance-check.py','link-echo.py','firmware-update-check.py','product-management-check.py')) {
    Copy-Required (Join-Path $projectRoot "scripts/$name") "tools/$name"
  }

}
Copy-Required (Join-Path $projectRoot 'scripts/provision-device.py') 'tools/provision-device.py'
Copy-Required (Join-Path $projectRoot 'dist/tools/provision-device.exe') 'tools/provision-device.exe'
Copy-Required (Join-Path $projectRoot 'dist/tools/esptool.exe') 'tools/esptool.exe'
Copy-Required (Join-Path $projectRoot 'packaging/esptool_entry.py') 'tools/source/packaging/esptool_entry.py'
Copy-Required (Join-Path $projectRoot 'packaging/offline-tools-requirements.txt') 'tools/source/packaging/offline-tools-requirements.txt'
Copy-Required (Join-Path $projectRoot 'packaging/sources/esptool-5.4.0.tar.gz') 'tools/source/packaging/sources/esptool-5.4.0.tar.gz'
Copy-Required (Join-Path $projectRoot 'scripts/build-offline-tools.ps1') 'tools/source/scripts/build-offline-tools.ps1'
Copy-Required (Join-Path $projectRoot 'scripts/provision-device.py') 'tools/source/scripts/provision-device.py'
Copy-Required (Join-Path $projectRoot 'scripts/acceptance.py') 'tools/acceptance.py'
Copy-Required (Join-Path $projectRoot 'dist/test-upstream.exe') 'tools/test-upstream.exe'
Copy-Required (Join-Path $projectRoot 'dist/link-bench.exe') 'tools/link-bench.exe'
Copy-Required (Join-Path $projectRoot 'dist/link-recovery.exe') 'tools/link-recovery.exe'
Copy-Required (Join-Path $projectRoot 'cmd/link-bench/README.md') 'tools/link-bench-README.md'
# Public release documentation is explicit; never collect local test reports.
foreach ($name in @('README.md','BUILDING.md','GETTING_STARTED.md','ARCHITECTURE.md','VALIDATION.md',
    'site/index.html','site/style.css','development/2026-09-17-open-source.md',
    'implementation/protocol.md','implementation/management-api.md','implementation/firmware.md',
    'implementation/product-ui-design.md','implementation/hardware-acceptance.md')) {
  Copy-Required (Join-Path $projectRoot "docs/$name") "docs/$name"
}
foreach ($name in @('LICENSE','THIRD_PARTY.md','CONTRIBUTING.md','SECURITY.md','CHANGELOG.md')) {
  Copy-Required (Join-Path $projectRoot $name) $name
}
Copy-Required (Join-Path $projectRoot 'integrations/deepseek-harness/README.md') 'integrations/deepseek-harness/README.md'
foreach ($name in @('README.md','partitions.csv','sdkconfig.defaults')) {
  $source = Join-Path $projectRoot "firmware/$name"
  if (Test-Path -LiteralPath $source) { Copy-Required $source "firmware/$name" }
}
$licenseDirectory = Join-Path $projectRoot 'packaging/licenses'
if (-not (Test-Path -LiteralPath $licenseDirectory)) { throw 'Missing third-party license notices.' }
Get-ChildItem -LiteralPath $licenseDirectory -File | ForEach-Object { Copy-Required $_.FullName ('licenses/' + $_.Name) }
# Sources are allowlisted above. Private pairing material is never copied into the payload.
$forbidden = Get-ChildItem -LiteralPath $stageDirectory -Recurse -File | Where-Object { $_.Name -match '(?i)pairing\.(json|csv|bin)$|credentials|\.pem$|\.key$' }
if ($forbidden) { throw 'Secret-like files found in staged release; refusing packaging.' }
$files = Get-ChildItem -LiteralPath $stageDirectory -Recurse -File | Sort-Object FullName
$hashes = foreach ($file in $files) {
  $relative = [IO.Path]::GetRelativePath($stageDirectory, $file.FullName).Replace('\','/')
  '{0}  {1}' -f (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant(), $relative
}
$hashes | Set-Content -LiteralPath (Join-Path $stageDirectory 'checksums.sha256') -Encoding utf8NoBOM
@{ name='uart2llm'; version=$Version; platform='windows-x64'; built_utc=[DateTime]::UtcNow.ToString('o'); hardware='ESP32-S3 16 MiB flash / 8 MiB PSRAM'; credentials_included=$false } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $stageDirectory 'manifest.json') -Encoding utf8NoBOM
$uninstallList = $stageDirectory + '-uninstall.nsh'
$uninstallCommands = @()
foreach ($file in (Get-ChildItem -LiteralPath $stageDirectory -Recurse -File)) {
  $relative = [IO.Path]::GetRelativePath($stageDirectory,$file.FullName)
  if ($relative -match '["$\r\n]') { throw 'A release filename is unsafe for installer generation.' }
  $uninstallCommands += 'Delete "$INSTDIR\' + $relative + '"'
}
foreach ($directory in (Get-ChildItem -LiteralPath $stageDirectory -Recurse -Directory | Sort-Object { $_.FullName.Length } -Descending)) {
  $relative = [IO.Path]::GetRelativePath($stageDirectory,$directory.FullName)
  $uninstallCommands += 'RMDir "$INSTDIR\' + $relative + '"'
}
$uninstallCommands | Set-Content -LiteralPath $uninstallList -Encoding utf8NoBOM
$zip = Join-Path $releaseDirectory "uart2llm-$Version-windows-x64.zip"
Compress-Archive -Path (Join-Path $stageDirectory '*') -DestinationPath $zip -Force
$setup = Join-Path $releaseDirectory "uart2llm-$Version-windows-x64-setup.exe"
if ($LinuxMakeNSIS) {
  if (-not $LinuxNSISDirectory) { throw 'LinuxNSISDirectory is required with LinuxMakeNSIS.' }
  $linuxStage = (& wsl -d $WSLDistro -- wslpath -u $stageDirectory.Replace('\','/')).Trim()
  $linuxOutput = (& wsl -d $WSLDistro -- wslpath -u $setup.Replace('\','/')).Trim()
  $linuxScript = (& wsl -d $WSLDistro -- wslpath -u (Join-Path $projectRoot 'packaging/windows.nsi').Replace('\','/')).Trim()
  $linuxUninstallList = (& wsl -d $WSLDistro -- wslpath -u $uninstallList.Replace('\','/')).Trim()
  & wsl -d $WSLDistro -- env "NSISDIR=$LinuxNSISDirectory" $LinuxMakeNSIS -WX -V2 "-DSTAGE_DIR=$linuxStage" "-DOUTPUT_FILE=$linuxOutput" "-DUNINSTALL_LIST=$linuxUninstallList" "-DAPP_VERSION=$Version" $linuxScript
} else {
  if (-not $MakeNSIS) {
    $command = Get-Command makensis.exe -ErrorAction SilentlyContinue
    if (-not $command) { throw "Portable ZIP built at $zip. Supply -MakeNSIS or -LinuxMakeNSIS to produce an installer." }
    $MakeNSIS = $command.Source
  }
  & $MakeNSIS /WX /V2 "/DSTAGE_DIR=$stageDirectory" "/DOUTPUT_FILE=$setup" "/DUNINSTALL_LIST=$uninstallList" "/DAPP_VERSION=$Version" (Join-Path $projectRoot 'packaging/windows.nsi')
}
if ($LASTEXITCODE -ne 0) { throw 'NSIS installer compilation failed.' }
Get-Item -LiteralPath $zip,$setup | Select-Object FullName,Length | Format-Table
Get-FileHash -LiteralPath $zip,$setup -Algorithm SHA256 | Format-List Path,Hash
