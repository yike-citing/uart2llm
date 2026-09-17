# 从源码构建

本指南面向 Windows 开发环境。应用运行不要求安装这些开发工具；首次构建需要下载锁定依赖，不能把首次源码构建称为离线安装。

## 依赖

| 组件 | 构建环境 |
|---|---|
| 后台 | Go ≥ 1.24；`go.mod` 为依赖入口 |
| Web | Node.js 22.12+，npm；使用 `web/package-lock.json` |
| 原生 GUI | CMake ≥ 3.24，C++17 Windows 工具链；wxWidgets 3.2.8 |
| 固件 | ESP-IDF 6.0.1、该版本配套 Python 与工具链 |
| 离线工具 | Python 3.11；`packaging/offline-tools-requirements.txt` |
| 安装程序 | PowerShell 7、NSIS |

从仓库根目录执行。不要先移动 `cmd`、`internal`、`web` 等目录；构建脚本根据仓库根目录定位资源。

## Web 与 Go 后台

```powershell
./scripts/build-ui.ps1
go test ./...
go vet ./...
New-Item -ItemType Directory -Force dist | Out-Null
go build -trimpath -ldflags "-s -w -X main.version=0.2.0-dev" -o dist/uart2llm.exe ./cmd/uart2llm
go build -trimpath -o dist/test-upstream.exe ./cmd/test-upstream
go build -trimpath -o dist/link-bench.exe ./cmd/link-bench
go build -trimpath -o dist/link-recovery.exe ./cmd/link-recovery
```

`build-ui.ps1` 执行锁定安装、TypeScript 检查、生产构建和前端测试，然后把资源放入 `internal/ui/assets` 供 Go 嵌入。源码中的占位页只用于让独立 Go 检查能够编译；发行构建必须先生成真实 Web。

## 原生 GUI

日常使用的单 EXE Agent 已内置 Web 管理与托盘，无需构建原生 GUI。构建完 Web 后，执行 `./scripts/build-agent.ps1 -Version 0.2.0-preview.1`，产物在 `dist/release`。双击自动启动并打开管理页；显式参数保留 CLI 用法。许可证通过 `licenses` 命令可从 EXE 内读取。

以下 wxWidgets 界面作为可选组件继续保留：

在带 MSVC 的开发终端：

```powershell
cmake -S native -B native/build -A x64
cmake --build native/build --config Release --parallel
ctest --test-dir native/build -C Release --output-on-failure
```

CMake 优先使用已安装 wxWidgets，否则获取固定版本并验证 SHA-256。如使用 LLVM-MinGW，设置 `LLVM_MINGW_ROOT` 并使用仓库中的 `native/toolchain-llvm-mingw.cmake`，不要复用其他电脑的构建缓存。

## ESP32-S3 固件

先打开 **ESP-IDF 6.0.1 PowerShell 环境**，确认板卡确实为 16 MiB Flash / 8 MiB Octal PSRAM。

GPIO UART：

```powershell
idf.py -C firmware set-target esp32s3
idf.py -C firmware menuconfig
idf.py -C firmware build
```

在 `uart2llm board wiring` 配置 GPIO。默认 UART1 TX17/RX18，RTS/CTS 未接；Flash/PSRAM 占用、引脚重叠等在构建中检查。

原生 USB 验证版本使用独立输出和配置：

```powershell
$fwDir = (Resolve-Path firmware).Path
idf.py -C $fwDir -B "$fwDir/build-usb" -D "SDKCONFIG=$fwDir/build-usb/sdkconfig" -D "SDKCONFIG_DEFAULTS=$fwDir/sdkconfig.usb-validation" build
```

也可在已激活 IDF 环境使用 `scripts/build-firmware.ps1 -Transport uart` 或 `usb-validation`。两种固件的业务接口不同；固件校验通过不代表目标接线经过实测。[烧录与首次配对](GETTING_STARTED.md)。

## 离线发行物

```powershell
./scripts/build-offline-tools.ps1 -Python python
./scripts/package.ps1 -Version 0.2.0-dev -NativeExe native/build/Release/uart2llm-gui.exe -MakeNSIS C:/Tools/NSIS/makensis.exe
```

要同时携带 USB 验证固件，先构建两种目标，再传 `-IncludeUSBValidation`。打包需要所有指定产物齐全，不包含每设备的配对文件。公开打包脚本携带公共文档，不收集本机硬件测试记录。

esptool 以独立工具形式分发；对应源码、入口脚本、构建方法和许可证都要保留，见 [第三方说明](../THIRD_PARTY.md)。此仓库尚未配置代码签名或公开下载地址。

## 不需要硬件的检查

```powershell
go test ./...
go vet ./...
npm --prefix web ci
npm --prefix web test
node --test integrations/deepseek-harness/scheduler.test.mjs
python -m unittest discover -s scripts -p "*test*.py"
python scripts/check-source.py
```

部分 Python 测试需要 `esp-idf-nvs-partition-gen`，实时测试另需 pyserial、websocket-client 或 Pillow；以脚本帮助和 `packaging/offline-tools-requirements.txt` 为准。Harness 安装器测试需要本机安装 Harness，不能用未安装环境的跳过代替该项通过。

生成源码归档：`python scripts/source-archive.py --output ../uart2llm-source.zip`。它按源码目录和文件类型清单归档，排除生成资产、缓存、设备凭据和原始测试输出；目标文件已存在时拒绝覆盖。

## GitHub Actions 发布

`.github/workflows/release.yml` 在推送 `v*` 标签时编译 Windows 单 EXE、独立设备工具和 UART/USB 两种固件；全部成功后生成 SHA-256 并发布 GitHub pre-release。仅发布任务持有 `contents: write`。PR 和手动执行只构建，不创建 Release。依赖 Actions 固定到提交 SHA。

Windows 检查包含前端、Go vet/测试、Python/调度器回归和独立 EXE 后台启动/管理/关闭验证。固件由 ESP-IDF 6.0.1 容器构建。云端检查通过仍不代表硬件全功能和持续负载验收完成。
