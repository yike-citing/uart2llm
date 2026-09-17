param(
    [ValidateSet('uart', 'usb-validation')][string]$Transport = 'uart',
    [string]$IdfPath = $env:IDF_PATH
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$firmwarePath = Join-Path $projectRoot 'firmware'
if (-not $IdfPath -or -not (Test-Path (Join-Path $IdfPath 'tools/idf.py'))) {
    throw 'Open an ESP-IDF 6.0.1 environment first, or pass -IdfPath after activating its tools.'
}
$env:IDF_PATH = $IdfPath
$pythonExe = (Get-Command python -ErrorAction Stop).Source
& $pythonExe (Join-Path $IdfPath 'tools/idf.py') --version
if ($LASTEXITCODE -ne 0) { throw 'ESP-IDF environment is not active.' }
if ($Transport -eq 'uart') {
    $buildPath = Join-Path $firmwarePath 'build'
    $configPath = Join-Path $firmwarePath 'sdkconfig'
    $defaults = 'sdkconfig.defaults'
} else {
    $buildPath = Join-Path $firmwarePath 'build-usb'
    $configPath = Join-Path $buildPath 'sdkconfig'
    $defaults = 'sdkconfig.usb-validation'
}
& $pythonExe (Join-Path $IdfPath 'tools/idf.py') -C $firmwarePath -B $buildPath '-D' "SDKCONFIG=$configPath" '-D' "SDKCONFIG_DEFAULTS=$defaults" build
if ($LASTEXITCODE -ne 0) { throw "ESP-IDF build failed ($LASTEXITCODE)." }
$usbEnabled = [bool](Select-String -LiteralPath $configPath -Pattern '^CONFIG_U2_USB_VALIDATION=y$' -Quiet)
if ($usbEnabled -ne ($Transport -eq 'usb-validation')) {
    throw 'Existing sdkconfig selects the wrong transport. Correct its U2_USB_VALIDATION setting before using this image.'
}
Get-FileHash (Join-Path $buildPath 'uart2llm.bin') -Algorithm SHA256
