# Windows 单 EXE Agent 预览版

日常使用只需下载 **`uart2llm-agent-windows-x64.exe`**，放在固定目录并双击。Agent 自动启动后台和托盘，打开本地管理页面；重复启动会使用现有后台。Web 页面已嵌入 EXE，无需安装 Python、Node、Go 或 wxWidgets，也无需联网下载运行依赖。

首次使用仍需准备并配对 ESP32-S3。管理默认 `http://localhost:8766`，模型 API 默认 `http://localhost:8765/v1`。配置上游地址、key 和设备 Wi-Fi 后，在 IDE 中填写本地 API 地址与本地 token。

## 下载项

| 文件 | 用途 |
|---|---|
| `uart2llm-agent-windows-x64.exe` | 日常运行的单文件 Agent，含 Web、托盘和 CLI |
| `uart2llm-device-tools-windows-x64.zip` | 首次烧录/配对所需的独立工具；包含运行依赖与对应源码 |
| `uart2llm-firmware-uart.zip` | GPIO UART 固件；默认 UART1 TX17/RX18，共地 |
| `uart2llm-firmware-usb-validation.zip` | 原生 USB Serial/JTAG 验证固件，与 GPIO UART 构建不同 |
| `LICENSES.txt` | Agent 第三方通知；EXE 的 `licenses` 命令也可读取 |
| `SHA256SUMS` | 所有发布附件的 SHA-256 |

设备要求 ESP32-S3、16 MiB Flash、8 MiB Octal PSRAM。首次烧录时选择匹配连接方式的一种固件，并保留该 ZIP 的目录结构；不要混用两种构建的分区与应用。串口驱动取决于硬件，工具包不包含未知 USB-UART 转换器驱动。

## 首次设备准备

把设备工具和选定固件分别解压。以下示例在固件解压目录执行，将工具路径和 COMx 替换为实际值：

```powershell
C:/uart2llm-tools/tools/provision-device.exe --output "$env:USERPROFILE/uart2llm-device-private"
C:/uart2llm-tools/tools/esptool.exe --chip esp32s3 --port COMx write-flash --flash-mode dio --flash-size 16MB --flash-freq 80m 0x0 bootloader/bootloader.bin 0x8000 partition_table/partition-table.bin 0xf000 ota_data_initial.bin 0x20000 uart2llm.bin 0x12000 "$env:USERPROFILE/uart2llm-device-private/pairing.bin"
```

随后双击 Agent，在页面中选择业务串口并导入 `pairing.json`。GPIO UART 使用外接转换器的业务口；USB 验证版使用开发板原生 USB 口。配对文件只归自己的设备使用，不公开分享。

## 命令行和退出

```powershell
./uart2llm-agent-windows-x64.exe version
./uart2llm-agent-windows-x64.exe token api
./uart2llm-agent-windows-x64.exe status
./uart2llm-agent-windows-x64.exe stop
```

关闭浏览器不停止代理；从托盘退出或使用 `stop`。默认配置在 `%APPDATA%/uart2llm`，凭据归当前 Windows 用户。此单文件不创建系统服务、虚拟网卡或系统路由。升级前先退出旧 Agent，再替换 EXE。

## 当前发布边界

这是 **pre-release**，不是全功能验收完成的稳定版。当前支持四类 OpenAI 兼容 API；Responses API 仍是后续计划。Harness 独立联网搜索、网页读取和完整离线/24 小时验收尚未完成；详情见仓库验证文档。

GitHub Actions 编译与自动化测试不能替代目标板接线、掉电与持续负载验证。EXE 当前未作 Authenticode 代码签名；安全软件处理取决于设备策略，不承诺免告警。
