<div align="center">

# uart2llm

**身在内网，开发照常。**

让没有公网出口的内网开发机，用上先进的云端大模型。

一根串口线接上 ESP32-S3，电脑上用标准 OpenAI 接口继续原来的开发流程。

Windows 10/11 · 单文件 Agent · 不创建虚拟网卡 · OpenAI 兼容接口 · MIT

[使用方式](#使用方式) · [核心亮点](#核心亮点) · [下载单 EXE Agent](https://github.com/yike-citing/uart2llm/releases) · [项目介绍页](docs/site/index.html) · [构建指南](docs/BUILDING.md) · [文档中心](docs/README.md)

</div>

---

[这是什么](#这是什么) · [为什么需要它](#为什么需要它) · [设计出发点](#设计出发点) · [工作方式](#工作方式) · [核心亮点](#核心亮点) · [使用方式](#使用方式) · [当前状态](#当前状态) · [从源码构建](#从源码构建) · [安全与合规边界](#安全与合规边界)

## 这是什么

内网开发机上，日常写代码离不开大模型，但"让这台机器用上大模型"往往不是技术问题，而是**通道问题**。

uart2llm 的做法是：**不碰电脑的网络，把出口搬到一根串口线另一端的 ESP32-S3 上。**

- 电脑侧只多出一个**串口设备**：不装虚拟网卡、不改路由表、不注册网络驱动、不创建系统服务、不添加防火墙规则。
- **联网由设备完成**：ESP32-S3 用 Wi-Fi 连接上游 OpenAI 兼容服务，只做 DNS 解析和 TCP 转发。
- **明文和密钥留在电脑上**：TLS 在电脑进程内建立并终结，串口里流动的只有密文。
- **工具侧零改造**：对外提供标准 OpenAI 兼容接口，IDE、聊天客户端和自动化脚本只需要改一个 Base URL。

```text
你的工具（IDE / 聊天客户端 / 脚本）
        │   http://localhost:8765/v1        OpenAI 兼容
        ▼
uart2llm Agent（单个 EXE，Go）
        │   · 四类模型 API 白名单转发
        │   · 在电脑上完成 TLS —— 明文只存在于本机进程内
        │   · 串口多路复用 / 流控 / 取消
        ▼
   USB 串口（COMx）
        ▼
ESP32-S3（配套硬件）
        │   · 只做 DNS + TCP，转发密文
        │   · Wi-Fi 连接上游
        ▼
   上游 OpenAI 兼容服务（api.openai.com 或自建/供应商地址）
```

## 为什么需要它

内网开发环境有一个结构性矛盾：**开发离不开大模型，但开发机往往没有可用的公网出口。**

常见替代路径的代价也不小：

| 路径 | 代价 |
|---|---|
| 内网自建推理服务 | 受内网算力限制，通常只能跑内网部署的模型，与先进云端模型的能力差距明显 |
| VPN / 虚拟网卡 / 私自加代理 | 新增网络接口、改动路由与系统网络配置，属于明显的对外联网行为 |
| 申请开通公网出口 | 流程长、审批不确定，解决不了"现在就要用"的问题 |

问题在于：**这台电脑缺的从来不是算力，而是一条不需要改动系统网络形态的对外通道。**

## 设计出发点

uart2llm 的通道选择不是随意的，而是针对内网开发机的一个工程约束：

- **让电脑自己联网，就要动电脑的网络。** VPN、虚拟网卡、代理都会新增网络接口、改动路由，或者产生新的对外连接。这是整条链路上唯一必须"改系统"的部分，也是内网管控真正盯住的部分。
- **串口本来就在这些机器上正常工作。** 硬件调试与烧录、串口打印机、硬件远程控制卡（KVM）都要用串口，系统里存在一个 COM 设备是常态，不需要为它改变任何网络配置。

终端管控的关注点通常也很集中：**大容量存储设备的接入与向外拷贝、未注册网卡或网卡状态变化、明显的数据外发行为**。相比之下，串口、键盘鼠标、USB 耳机、蓝牙这类外设在多数配置下被当作"正常工作负载"处理——但**是否产生告警最终取决于你所在环境的监控软件与组织策略，本项目不承诺免告警**。

所以设计目标是：**把"联网"这件事从电脑的网卡上移走，交给一根 USB 线另一端的小盒子。**

1. 电脑侧不新增网络接口、不改路由、不注册网络驱动；
2. 对操作系统而言，这台电脑只是接了一个串口设备；
3. 真正联网的是 ESP32-S3，它不持有上游 API key，也看不懂流经它的字节。

**这个项目的创新点**：用一根串口线，为没有公网出口的电脑补上对外模型访问能力——不改变电脑自身的网络形态，也不把明文和密钥交给外部硬件。

同时必须讲清它的边界：**uart2llm 不修改、不关闭、不绕过任何安全软件，也不隐藏自身。** 没有注入、没有提权、没有进程伪装；Agent 是一个可见的普通用户态进程，设备是一个可见的串口设备，链路上也没有为规避检测而设计的特性。请在允许使用的设备与外部服务范围内使用，并自行确认符合组织的安全政策与适用法律，详见[安全与合规边界](#安全与合规边界)。

## 工作方式

**一次请求的完整旅程：**

1. IDE 或工具向 `http://localhost:8765/v1/chat/completions` 发起一个标准 OpenAI 请求，用本地 token 认证。
2. Agent 校验路径在白名单内，取出配置好的上游地址与凭据，**在电脑上发起并终结 TLS**。
3. 加密后的字节流被封成串口帧，经 COM 口送往 ESP32；链路层负责分帧、确认、去重、流控、多路复用与取消。
4. ESP32 解析 DNS 并转发 TCP，把密文送上 Wi-Fi 可达的上游服务——它看不到请求内容。
5. 上游响应原路返回电脑，由 Agent 解密后以 SSE 流式（或非流式）交回客户端。

**两个视角，同一根线：**

| 视角 | 看到的东西 |
|---|---|
| 操作系统与监控软件 | 一个普通的 USB 串口设备（COMx），系统网络配置没有任何变化。原生 USB Serial/JTAG 固件下该设备同时提供调试接口 |
| 数据本身 | 密文：串口里只有 TLS 记录，prompt、工具参数、文件内容、SSE 事件和 API key 都不上串口 |

## 核心亮点

### 1. 一根串口替代一块网卡，电脑侧零网络改动

不安装虚拟网卡、不修改系统路由、不注册网络驱动、不创建系统服务、不添加防火墙规则。Agent 只是一个使用串口的普通用户态进程。**结果是**：它不会与既有的网络配置、VPN 客户端或网络策略管理器冲突，卸载时也不留残渣——因为系统网络层面本来就没有被改动过。

### 2. 明文不出电脑：设备只搬运密文

这是与"把请求交给外置设备处理"最本质的区别。**TLS 连接在电脑上建立和终结**，ESP32 只提供一条到上游的 DNS + TCP 通道，它无法解析 prompt、工具参数、文件内容或 SSE 事件，也无法读取你的上游 API key。**结果是**：提示词与响应正文只在电脑进程内出现，对外联网能力被限制在一个不持有秘密的转发器里；设备上只保留 Wi-Fi 凭据与设备配置，即使设备被别人拿走，也没有可提取的对话内容或上游 key。

### 3. 单文件 EXE，可携带进入内网

日常运行只需要一个 `uart2llm-agent-windows-x64.exe`：**Web 管理页、系统托盘和 CLI 全部内置**，不需要安装 Python、Node.js、Go 或 ESP-IDF，也不需要联网获取运行依赖。提前把 EXE、设备工具和固件拷进内网，**使用发布包时整个部署过程不需要外网**。

> 首次源码构建需要拉取依赖，但**使用**不需要——两者的区别见[发行说明](docs/RELEASE.md)。

### 4. 标准 OpenAI 接口，现有工作流不用改

提供四类 OpenAI 兼容 API：`/v1/chat/completions`、`/v1/models`、`/v1/files`、`/v1/fine_tuning/jobs`，支持 SSE 流式与非流式。任何支持自定义服务地址的工具都能接入，**无需迁移到专用客户端，也无需改变使用习惯**。白名单之外的路径返回明确的 `unsupported_endpoint`，不会静默失败。

### 5. 密钥分层：配对凭据、上游 key、本地 token 各归其位

| 环节 | 设计 |
|---|---|
| 设备配对 | Protocomm Security 2 握手，**每台设备单独生成**配对凭据 |
| 配对文件 | 保存在仓库之外，不提交 Git、不随安装包分发 |
| 上游 API key | 存放在 **Windows 凭据管理器**，不写入配置文件 |
| key 输入方式 | 仅通过 stdin（`credentials set-upstream`），不接受命令行明文 |
| 本地访问控制 | 管理页面仅回环监听、面向本机用户；模型 API 使用**独立**的本地 token |
| 日志 | 默认不记录请求正文与秘密 |

**结果是**：模型 API 的 token、设备配对密码、上游 key 三者互不通用，任何一份泄露都不会直接等于另外两份泄露。

### 6. 配置事务：可确认、可回滚

设备配置采用**暂存 → 应用 → 确认**流程。应用 Wi-Fi 后先检查是否取得 IP，再在 **30 秒窗口内确认，超时自动回滚**；配置变更前会等待活动请求结束。**结果是**：一次写错的网络配置不会把设备变成砖。

### 7. 四路并发、有界背压、写操作不重放

链路层实现分帧、确认、去重、流控、多路复用与取消，**最多四条上游请求**并发——也就是同时最多四个模型会话；第五个请求会立刻拿到 `503 capacity_exceeded` 与 `Retry-After`，而不是排队卡死等超时。

管理、遥测、日志、OTA 与四条 TCP 通道分别排队，链路层对端到端慢消费者施加**有界队列背压**，而不是无限缓冲。**结果是**：多个 IDE 会话同时工作时不会互相拖垮，中途取消后连接立即失效并可继续发起新请求。

写操作**不自动重试**——串口拔线或设备重启会让旧请求失败，后台不会重放可能已经发送出去的生成、上传或创建任务。

### 8. 链路全程可观测

- **图形化数据看板**：请求数量、成功与失败、响应时间、吞吐及 Token 用量。
- **设备实时状态**：Wi-Fi 信号强度、IP、DNS、断开原因、重连次数、各通道状态与 TCP_NODELAY。
- **托盘快捷管理**：查看连接与模型统计，暂停、恢复和管理服务。

统计以当前进程实际采集的数据为准，**缺失值不视为零**。

## 使用方式

**五步走完：**

| 步骤 | 做什么 | 详细位置 |
|---|---|---|
| 1 | 准备硬件与文件：ESP32-S3（16 MiB Flash、8 MiB Octal PSRAM）、3.3 V USB-UART 转换器、Agent EXE | [第 1 步](#第-1-步准备) |
| 2 | 烧录固件，并生成本机专属配对凭据 | [第 2 步](#第-2-步烧录固件并生成配对凭据) |
| 3 | 启动 Agent，连接串口并导入 `pairing.json` | [第 3 步](#第-3-步启动-agent-并连接串口) |
| 4 | 在管理页面填写 Wi-Fi 与上游（Base URL + 上游 key） | [第 4 步](#第-4-步配置-wi-fi-与上游) |
| 5 | 在 IDE 里填 `http://localhost:8765/v1` 和本地 token | [第 5 步](#第-5-步接入-ide-或客户端) |

### 第 1 步：准备

| 项目 | 要求 |
|---|---|
| 电脑 | Windows 10/11 x64 |
| 设备 | ESP32-S3，**16 MiB Flash、8 MiB Octal PSRAM** |
| 连接 | GPIO UART（外接 3.3 V USB-UART 转换器）**或**原生 USB Serial/JTAG |
| 网络 | ESP32 所连 Wi-Fi 必须能直接访问上游服务 |
| 文件 | 日常运行只需要 Agent EXE；首次烧录另需固件与设备工具（见[发行说明](docs/RELEASE.md)下载项） |

> GPIO UART 默认 TX17/RX18，需**交叉接线并共地**（ESP TX → 转换器 RX，ESP RX → 转换器 TX，GND ↔ GND）。原生 USB Serial/JTAG 需要烧录**独立的 USB 验证固件**，它与生产 GPIO UART 固件不同，两种构建的 bootloader、分区表与应用**不可混用**。

> **`dist\uart2llm.exe` 与 `uart2llm-agent-windows-x64.exe` 是同一个程序的两种来源**：前者是源码构建产物（见[从源码构建](#从源码构建)），后者是发布包里的单文件 EXE。下面的命令对两者都适用，按你的实际文件名替换即可。

### 第 2 步：烧录固件并生成配对凭据

每台设备单独生成凭据，`COMx` 替换为实际烧录口。**推荐用发布包的设备工具**，它不依赖 ESP-IDF：

在解压后的固件目录中执行（`C:/uart2llm-tools` 替换为你的工具解压目录）：

```powershell
C:/uart2llm-tools/tools/provision-device.exe --output "$env:USERPROFILE/uart2llm-device-private"
C:/uart2llm-tools/tools/esptool.exe --chip esp32s3 --port COMx write-flash --flash-mode dio --flash-size 16MB --flash-freq 80m 0x0 bootloader/bootloader.bin 0x8000 partition_table/partition-table.bin 0xf000 ota_data_initial.bin 0x20000 uart2llm.bin 0x12000 "$env:USERPROFILE/uart2llm-device-private/pairing.bin"
```

从源码构建固件时，改用 ESP-IDF 环境：

```powershell
python scripts/provision-device.py --output "$env:USERPROFILE\uart2llm-device-private"
idf.py -C firmware -p COMx flash
python -m esptool --chip esp32s3 --port COMx write-flash 0x12000 "$env:USERPROFILE\uart2llm-device-private\pairing.bin"
```

`pairing.json` 与 `pairing.bin` 保存在仓库外，**不要提交或分享**。重新烧录配对分区会更换设备配对身份。

### 第 3 步：启动 Agent 并连接串口

最简单的方式是**双击 EXE**：它会自动启动后台、进入系统托盘并打开管理页面；重复启动会复用已有后台。也可以用命令行：

```powershell
.\dist\uart2llm.exe init        # 初始化配置目录与本地 token
.\dist\uart2llm.exe start       # 启动后台（detached）
.\dist\uart2llm.exe ports       # 列出可用串口
.\dist\uart2llm.exe connect --port COM5 --baud 115200
.\dist\uart2llm.exe pair "$env:USERPROFILE\uart2llm-device-private\pairing.json"
.\dist\uart2llm.exe status      # 查看运行状态
```

RTS/CTS 硬件流控可加 `--rtscts`。**GPIO UART 每次启动回到 115200/8N1/无流控，连接后可协商提高速率；原生 USB Serial/JTAG 路径不受波特率设置影响**——给 COM 端口设 2,000,000 波特率不会提高 USB 物理速率。

> 后台会独占串口，**烧录前先断开后台的串口连接**，否则烧录会失败。

### 第 4 步：配置 Wi-Fi 与上游

打开管理页面 **`http://localhost:8766`**，在分组表单中填写：

| 分组 | 填写内容 |
|---|---|
| 网络 | Wi-Fi SSID、密码（可先扫描可用网络） |
| 上游 | Base URL（如 `https://api.openai.com/v1`）与上游 API key |

上游 key 也可以在命令行写入 **Windows 凭据管理器**：

```powershell
.\dist\uart2llm.exe credentials set-upstream < key.txt
```

应用 Wi-Fi 后**检查是否已取得 IP，并在 30 秒内确认**；超时自动回滚。ESP32 所连 Wi-Fi 必须能直连上游，电脑时间与根证书需支持 TLS 验证。

### 第 5 步：接入 IDE 或客户端

| 客户端配置 | 填写内容 |
|---|---|
| Base URL | `http://localhost:8765/v1` |
| API key | 下面命令输出的本地 token |
| 模型 | 获取模型列表后，选择上游实际可用的模型 |

```powershell
.\dist\uart2llm.exe token api
```

> - **不要**把设备配对密码当作 API key。
> - 部分客户端要求地址不带 `/v1`，请按该客户端的拼接方式配置，避免出现 `/v1/v1`。
> - 工具调用在**客户端**执行。这个端点只转发上述四类模型 API，不代理除此之外的任何接口。

## 命令行参考

```text
uart2llm - UART OpenAI gateway
Commands:
  init | serve | start | stop | status | tray | ports | version | licenses
  connect --port COM3 --baud 115200 [--rtscts]
  pair pairing.json
  token api|admin
  credentials set-upstream < key.txt
  config get|schema|confirm|rollback|set patch.json
  firmware firmware.bin
  admin METHOD /path [json-file|-]
Configuration directory: UART2LLM_DATA_DIR or %APPDATA%/uart2llm.
API defaults to http://localhost:8765/v1; management to http://localhost:8766.
```

还可以用 `uart2llm help` 查看同一份说明。模型 API 默认 `http://localhost:8765/v1`，管理页面默认 `http://localhost:8766`；配置目录默认为 `%APPDATA%\uart2llm`，可用环境变量 `UART2LLM_DATA_DIR` 覆盖。

## 运行状态、恢复与升级

- **关闭浏览器不会停止代理**。停止请用托盘退出或 `uart2llm.exe stop`。
- 串口拔线或设备重启会使旧 TCP/TLS 请求失败，重连后接受新请求；后台**不会自动重放**写操作。
- 升级前先结束活动请求，选择与连接方式匹配的固件镜像。候选固件需启动自检确认；**SHA-256 与格式检查不等同于发布者签名**，默认不烧写 Secure Boot / Flash Encryption eFuse。

## 常见问题

| 现象 | 原因与处理 |
|---|---|
| 客户端报 `/v1/v1` 或 404 | 客户端自己会补 `/v1`，Base URL 里去掉 `/v1` 再试 |
| 模型 API 返回 401 | 用了设备配对密码，或用了上游 key。这里要填 `token api` 输出的**本地 token** |
| 同时开多个会话时返回 `503 capacity_exceeded` | 链路上限为四条并发上游请求。稍后重试，或在客户端侧限制并发 |
| 烧录失败 / 找不到端口 | 后台正独占串口，先 `\dist\uart2llm.exe stop` 或断开连接 |
| 应用 Wi-Fi 后设备失联 | 30 秒内没有确认，配置已自动回滚；重新连接后检查 SSID、密码与信道 |
| 上游连接失败 | ESP32 所连 Wi-Fi 必须能直连上游；同时检查电脑时间与根证书是否支持 TLS 验证 |
| 某些接口返回 404 | 上游不保证实现全部四类 API（已观测到部分供应商的文件与微调接口返回 404），见[验证状态](docs/VALIDATION.md) |
| 模型列表为空或与预期不符 | 模型列表来自上游。先在一台能上网的机器上请求该上游的 `/v1/models`，确认账号实际可见的模型清单 |
| Harness 的搜索 / 网页读取不可用 | 这两项不在四类模型 API 内，需要走额外接口，当前本地路由未提供 |

## 当前状态

当前发布为 **`v0.2.0-preview.1` 预览版**，**全功能验收尚未通过**。我们宁可把边界写清楚，也不把预览版说成稳定版。

下面所有结论都来自**特定开发设备、版本与负载**，不是对任意模型、客户端或硬件的兼容保证；某项"已验证"只代表该项在它的测试范围内通过。

**已验证（在各自测试范围内）：**

- 主机协议、CRC/去重、四路容量、取消、网关转发、安全握手与管理 API 有自动化测试。
- Windows 原生 USB Serial/JTAG 链路已通过 ESP32 实板测试，含双向大块校验、背压、配置回滚与 Wi-Fi 恢复。**该结论不替代外接 GPIO UART 的接线、波特率与 RTS/CTS 验收。**
- 已通过本地网关调用**真实模型**，覆盖 SSE、文件传输及客户端代理任务。
- 已在 DeepSeek Harness Web 中运行真实会话：standard 27 个工具中 25 个成功执行，子代理另执行了 `structured_output`。
- 容量冲突实验：四个主会话加四个标题请求超过四路上限，加入客户端有界调度后，最终 8/8 请求成功、活动峰值 4，中途取消后可继续请求。这是该次实验的结论，不是持续保证。

**尚未通过：**

| 项目 | 状态 |
|---|---|
| Harness `web_search` | 原生搜索走额外 Messages 接口，当前本地路由返回 404 |
| Harness `web_fetch` | 现有插件使用电脑 HTTP，ESP32 通道未提供 |
| Harness 页面交互与其他预设 | 后台认证接口已验证；浏览器完整交互及 PTC/minimal/cordis 预设尚未验收 |
| 全部上游 API 语义 | 上游各异；已观测到 DeepSeek 部分文件与微调操作返回 404 |
| Responses API | 仍属后续计划 |
| 电脑物理无其他网络出口 | 独立物理隔离验收未完成 |
| 24 小时四路混合负载 | 未完成 |
| 拔线、物理掉电、升级掉电恢复 | 完整矩阵未完成 |
| SPI / Linux / macOS | 首阶段不提供 |

完整结论与复验方法见[验证状态](docs/VALIDATION.md)。

## 从源码构建

运行不要求开发工具；**首次源码构建需要下载锁定依赖**，不能称为离线安装。

| 组件 | 构建环境 |
|---|---|
| 后台 | Go ≥ 1.24 |
| Web | Node.js 22.12+、npm |
| 原生 GUI（可选） | CMake ≥ 3.24、C++17、wxWidgets 3.2.8 |
| 固件 | ESP-IDF 6.0.1 及配套 Python 与工具链 |
| 离线工具 | Python 3.11 |
| 安装程序与发布包 | PowerShell 7、NSIS |

```powershell
./scripts/build-ui.ps1
go test ./...
go vet ./...
New-Item -ItemType Directory -Force dist | Out-Null
go build -trimpath -ldflags "-s -w -X main.version=0.2.0-dev" -o dist/uart2llm.exe ./cmd/uart2llm
```

上面得到的是**开发版**（版本号 `0.2.0-dev`）。要生成与发布包同源的版本化产物，用 `./scripts/build-agent.ps1 -Version 0.2.0-preview.1`，输出在 `dist/release`。

固件（在 ESP-IDF 6.0.1 环境中）：

```powershell
idf.py -C firmware set-target esp32s3
idf.py -C firmware menuconfig      # 在 "uart2llm board wiring" 配置 GPIO
idf.py -C firmware build
```

无需硬件的完整检查：

```powershell
go test ./... && go vet ./...
npm --prefix web ci && npm --prefix web test
node --test integrations/deepseek-harness/scheduler.test.mjs
python -m unittest discover -s scripts -p "*test*.py"
python scripts/check-source.py
```

详见[构建指南](docs/BUILDING.md)。

## 文档

| 我想要… | 文档 |
|---|---|
| 烧录、配对和接入客户端 | [首次使用](docs/GETTING_STARTED.md) |
| 下载单 EXE 和设备工具 | [发行说明](docs/RELEASE.md) |
| 编译源码和生成 Windows 包 | [构建指南](docs/BUILDING.md) |
| 理解模块边界 | [架构与项目结构](docs/ARCHITECTURE.md) |
| 查看实际验证与未完成项 | [验证状态](docs/VALIDATION.md) |
| 调用管理 API | [接口契约](docs/implementation/management-api.md) |
| 修改协议或固件 | [二进制协议](docs/implementation/protocol.md)、[固件设计](docs/implementation/firmware.md) |
| 运行硬件验收与 24 小时压力测试 | [硬件验收方法](docs/implementation/hardware-acceptance.md) |
| 接入 DeepSeek Harness | [可选集成](integrations/deepseek-harness/README.md) |
| 贡献或报告安全问题 | [贡献指南](CONTRIBUTING.md)、[安全说明](SECURITY.md) |

## 安全与合规边界

- Agent 通过串口与 ESP32 通信，**不在 Windows 中创建网络接口、不安装虚拟网卡、不修改系统路由、不注册网络驱动、不创建系统服务、不添加防火墙规则**。
- **管理网页免密码进入**，只面向同一 Windows 用户的回环环境，不是远程多人控制面板；**不要将管理端口暴露到局域网或互联网**。模型 API 仍单独使用本地 token 认证。
- **是否产生安全告警取决于本机安全软件与组织策略，本项目不承诺免告警**。EXE 当前未作 Authenticode 代码签名。
- 模型正文以密文穿过串口；设备不解析 prompt、工具参数、文件内容或 SSE 事件，也不持有上游 API key。设备上保留 Wi-Fi 凭据与设备配置。
- 上游 API key 保存在 Windows 凭据管理器；Wi-Fi 凭据保存在设备配置中；日志默认不记录正文与秘密。
- 请仅在所在环境允许使用的设备和外部服务访问范围内使用，并自行确认符合组织的安全政策与适用法律法规。**本项目不用于规避你无权规避的安全管控。**

## 许可

原创代码采用 [MIT](LICENSE)。第三方组件保留各自许可，见 [THIRD_PARTY.md](THIRD_PARTY.md)。EXE 内嵌第三方通知，也可用 `uart2llm licenses` 读取。
