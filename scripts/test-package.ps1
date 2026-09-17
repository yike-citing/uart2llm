#Requires -Version 7.0
param([string]$Version='0.1.0', [switch]$InstallSmoke)
$ErrorActionPreference='Stop'
$projectRoot=[IO.Path]::GetFullPath((Split-Path $PSScriptRoot -Parent))
$zipPath=Join-Path $projectRoot "dist/uart2llm-$Version-windows-x64.zip"
$setupPath=Join-Path $projectRoot "dist/uart2llm-$Version-windows-x64-setup.exe"
$archive=[IO.Compression.ZipFile]::OpenRead($zipPath)
$checksums=@{}
try {
  $required=@('uart2llm.exe','uart2llm-gui.exe','README.md','checksums.sha256','manifest.json','firmware/uart2llm.bin','firmware/bootloader/bootloader.bin','firmware/partition_table/partition-table.bin','firmware/ota_data_initial.bin','tools/provision-device.exe','tools/esptool.exe','docs/implementation/management-api.md','docs/implementation/firmware.md','docs/implementation/validation.md','docs/implementation/spec-v1.md')
  $entries=@{}
  foreach($entry in $archive.Entries) {
    $name=$entry.FullName.Replace('\','/')
    if($name -match '(^|/)(pairing\.(json|csv|bin)|credentials[^/]*|[^/]+\.(pem|key))$'){throw "Secret-like entry in package: $name"}
    if($name -match '(^|/)\.\.(/|$)' -or $name.StartsWith('/') -or $name.Contains(':')){throw 'Unsafe archive path'}
    $entries[$name]=$entry
  }
  foreach($name in $required){if(-not $entries.ContainsKey($name)){throw "Missing package entry: $name"}}
  $reader=[IO.StreamReader]::new($entries['checksums.sha256'].Open())
  try { $lines=$reader.ReadToEnd() -split '\r?\n' } finally { $reader.Dispose() }
  foreach($line in $lines) {
    if(-not $line){continue}
    if($line -notmatch '^([0-9a-f]{64})  (.+)$'){throw 'Malformed checksum line'}
    $expected=$Matches[1];$name=$Matches[2]
    if(-not $entries.ContainsKey($name)){throw "Checksummed file is missing: $name"}
    $stream=$entries[$name].Open();$sha=[Security.Cryptography.SHA256]::Create()
    try{$actual=[Convert]::ToHexString($sha.ComputeHash($stream)).ToLowerInvariant()}finally{$stream.Dispose();$sha.Dispose()}
    if($actual -ne $expected){throw "Checksum mismatch: $name"}
    $checksums[$name]=$expected
  }
  foreach($name in $entries.Keys){if(-not $name.EndsWith('/') -and $name -notin @('checksums.sha256','manifest.json') -and -not $checksums.ContainsKey($name)){throw "Unchecksummed file: $name"}}
} finally { $archive.Dispose() }
Write-Output "ZIP verification passed: $($checksums.Count) payload hashes, required files, no bundled pairing secrets."
if(-not $InstallSmoke){return}
$registryKeys=@('HKCU:\Software\uart2llm','HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm')
$shortcutDirectory=Join-Path ([Environment]::GetFolderPath('Programs')) 'uart2llm'
if(($registryKeys | Where-Object {Test-Path -LiteralPath $_}) -or (Test-Path -LiteralPath $shortcutDirectory)){throw 'Existing product installation detected; refusing to modify it during smoke test.'}
if(Get-Process -Name uart2llm,uart2llm-gui -ErrorAction SilentlyContinue){throw 'An existing daemon or GUI is running; smoke installation will not interrupt it.'}
$testDirectory=[IO.Path]::GetFullPath((Join-Path $projectRoot ('.tools/installer-smoke-'+[guid]::NewGuid().ToString('N'))))
$allowedPrefix=[IO.Path]::GetFullPath((Join-Path $projectRoot '.tools'))+[IO.Path]::DirectorySeparatorChar
if(-not $testDirectory.StartsWith($allowedPrefix,[StringComparison]::OrdinalIgnoreCase)){throw 'Test install directory escaped workspace.'}
$installer=Start-Process -FilePath $setupPath -ArgumentList "/S /D=$testDirectory" -WindowStyle Hidden -PassThru
if(-not $installer.WaitForExit(30000)){Stop-Process -Id $installer.Id -Force;throw 'Installer did not complete within 30 seconds.'}
if($installer.ExitCode -ne 0){throw "Installer failed: $($installer.ExitCode)"}
foreach($name in $checksums.Keys){$path=Join-Path $testDirectory $name;if((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $checksums[$name]){throw "Installed payload mismatch: $name"}}
$userFile=Join-Path $testDirectory 'docs/implementation/smoke-user-note.txt'
'This test-created file must survive uninstall.' | Set-Content -LiteralPath $userFile
$uninstaller=Start-Process -FilePath (Join-Path $testDirectory 'Uninstall.exe') -ArgumentList '/S' -WindowStyle Hidden -PassThru
if(-not $uninstaller.WaitForExit(30000)){Stop-Process -Id $uninstaller.Id -Force;throw 'Uninstaller parent timed out.'}
$deadline=[DateTime]::UtcNow.AddSeconds(20)
while((Test-Path -LiteralPath (Join-Path $testDirectory 'uart2llm.exe')) -and [DateTime]::UtcNow -lt $deadline){Start-Sleep -Milliseconds 200}
if(Test-Path -LiteralPath (Join-Path $testDirectory 'uart2llm.exe')){throw 'Uninstaller left the application executable.'}
if(-not (Test-Path -LiteralPath $userFile)){throw 'Uninstaller removed a user-added file.'}
if(($registryKeys | Where-Object {Test-Path -LiteralPath $_}) -or (Test-Path -LiteralPath $shortcutDirectory)){throw 'Uninstaller left product registry entries or shortcuts.'}
if(Get-Process -Name uart2llm,uart2llm-gui -ErrorAction SilentlyContinue){throw 'Smoke test left an application process running.'}
# This random directory was created by this test under the already verified workspace prefix.
Remove-Item -LiteralPath $testDirectory -Recurse -Force
Write-Output 'Silent per-user install/uninstall passed; payload hashes matched, user-added file preserved, registry/shortcuts cleaned, no daemon left running.'
