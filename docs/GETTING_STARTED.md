# 首次使用

## 1. 选择连接方式

GPIO UART 使用 3.3 V 电平 USB-UART 转换器：ESP TX 接转换器 RX，ESP RX 接转换器 TX，共地。默认 TX17/RX18，可在固件板级菜单修改。普通 UART 每次启动回到 115200 / 8N1 / 无流控，连接后可协商提高速率。

原生 USB Serial/JTAG 必须烧录独立 USB 验证固件。它与生产 GPIO UART 固件不同；给 COM 设置 2,000,000 波特率不会使 USB 物理速率提高。USB 验证版禁用同口控制台输出，不能同时作为普通日志串口使用。

## 2. 烧录和生成自己的配对凭据

先完成[源码构建](BUILDING.md)。在 IDF 环境中，`COMx` 替换为本板烧录口：

```powershell
python scripts/provision-device.py --output "$env:USERPROFILE\uart2llm-device-private"
idf.py -C firmware -p COMx flash
python -m esptool --chip esp32s3 --port COMx write-flash 0x12000 "$env:USERPROFILE\uart2llm-device-private\pairing.bin"
```

以上 `idf.py flash` 对应 GPIO UART 构建。USB 版在相同的 IDF 配置参数下选择 `firmware/build-usb` 后执行 flash，不要混用另一构建的 bootloader、分区表与应用。固件分区以 `firmware/partitions.csv` 为准。

每台设备单独生成凭据。`pairing.json` 保存在仓库外，不提交到 Git，不随安装包分发。重新烧录配对分区会更换配对身份。

## 3. 启动与配对

```powershell
.\dist\uart2llm.exe init
.\dist\uart2llm.exe start
.\dist\uart2llm.exe connect --port COMx --baud 115200
.\dist\uart2llm.exe pair "$env:USERPROFILE\uart2llm-device-private\pairing.json"
```

GPIO UART 使用外接转换器的业务端口；USB 验证版使用开发板原生 USB 端口。后台独占端口，烧录前先断开后台串口连接。

## 4. 配置网络和上游

打开 `http://localhost:8766`。管理页面面向本机用户免密码进入；模型 API 仍单独认证。通过分组表单填写 Wi-Fi、上游 Base URL 和上游 key。

设备配置有暂存、应用和确认步骤。应用 Wi-Fi 后检查已获取 IP，再在 30 秒窗口内确认；超时回滚。配置变更前完成活动请求。上游地址示例：`https://api.openai.com/v1`，或供应商提供的 OpenAI 兼容地址。ESP32 所连 Wi-Fi 必须能直接访问上游；电脑时间与根证书必须支持验证 TLS。

配置目录默认 `%APPDATA%/uart2llm`，上游 key 由 Windows 凭据管理器保存。普通用户无需编辑 JSON。管理监听应保持在回环地址；本项目没有多人账户或远程管理隔离。

## 5. IDE 和模型客户端

Base URL 填 `http://localhost:8765/v1`。API key 填以下命令输出的本地 token：

```powershell
.\dist\uart2llm.exe token api
```

在客户端获取模型列表后选择上游可用模型。不要使用设备配对密码作为 API key。部分客户端要求地址不带 `/v1`，应按该客户端的拼接方式配置，避免出现 `/v1/v1`。

工具调用在客户端执行。模型端点不代理客户端自己的搜索、网页读取和插件安装。DeepSeek Harness 接入另见[集成说明](../integrations/deepseek-harness/README.md)。

## 6. 状态与恢复

托盘可查看设备连接与用量，Web 看板提供活动请求、吞吐、响应时间和设备资源。统计以当前进程实际采集数据为准，缺失值不视为零。

关闭界面不停止后台。停止：`uart2llm.exe stop`。串口拔线或设备重启会使旧 TCP/TLS 请求失败，重新连接后接受新请求；后台不自动重放可能已发送的生成、上传或创建任务。

升级前结束活动请求，选择匹配连接方式的应用镜像。候选固件需要启动自检确认；SHA-256 与格式检查不等同于固件发布者签名，默认不烧写 Secure Boot/Flash Encryption eFuse。
