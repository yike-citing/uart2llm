<div align="center">

# uart2llm

**身在内网，开发照常。**

为互联网访问不便、受限于内网开发环境的开发者准备。

Windows · 轻量 Agent · 离线安装 · OpenAI 兼容接口

[开始使用](docs/GETTING_STARTED.md) · [项目介绍页](docs/site/index.html) · [构建指南](docs/BUILDING.md) · [文档中心](docs/README.md)

[下载 Windows 单 EXE Agent](https://github.com/yike-citing/uart2llm/releases)

</div>

---

uart2llm 通过一个轻量本地 Agent 和配套 ESP32，为你的电脑提供模型 API 入口。ESP32 使用 Wi-Fi 连接上游模型服务，让不方便直接访问互联网的开发环境，也能接入熟悉的 AI 工具。

## 不改变现有工作流

提供标准 OpenAI 兼容 API，可接入支持自定义服务地址的 IDE、聊天客户端和开发工具。配置本地地址和 API key，继续使用原有的模型工作流，无需迁移到专用客户端。

| 客户端配置 | 填写内容 |
|---|---|
| Base URL | `http://localhost:8765/v1` |
| API key | `uart2llm-agent-windows-x64.exe token api` 输出的本地 token |
| 模型 | 已配置上游提供的模型 |

当前支持 Chat Completions 等接口，**后续计划兼容 Responses API**。具体兼容程度取决于客户端使用的接口和上游能力。

## 一个轻量 Agent，离线完成安装

Windows Agent 以**单个 EXE** 提供，内置管理页面与运行依赖。下载后双击即可启动，无需安装步骤或联网获取依赖，也无需额外安装 Python、Node、Go 或 ESP-IDF。提前准备好 EXE，即可带入内网环境使用。

连接配套 ESP32，在本地管理页面完成 Wi-Fi、上游地址和凭据配置，然后接入现有工具。关闭管理界面不会停止 Agent，常用操作也可从系统托盘完成。

首次烧录与配对所需的设备工具、两种固件在 [Release](https://github.com/yike-citing/uart2llm/releases) 单独提供，日常运行只需 Agent EXE。详细下载说明见[发行说明](docs/RELEASE.md)。首次源码构建需要获取依赖，设备准备与串口驱动要求见[首次使用](docs/GETTING_STARTED.md)。

## 不创建系统网络接口

Agent 通过串口与 ESP32 通信，不在 Windows 中创建网络接口、不安装虚拟网卡，也不修改系统路由。现有系统网络配置保持不变。

是否产生安全告警取决于本机安全软件与组织策略，本项目不承诺免告警。请在所在环境允许的设备和外部服务访问范围内使用。

## 完善的数据遥测与统计

- **图形化数据看板**：请求数量、成功与失败、响应时间、吞吐及 Token 用量。
- **设备实时状态**：Wi-Fi 信号、连接情况、流量和资源使用。
- **托盘快捷管理**：查看连接与模型统计，暂停、恢复和管理服务。
- **清晰的配置表单**：按设备、网络和模型服务分组，无需手动编辑内部配置。

本地管理入口默认 `http://localhost:8766`，免密码进入；模型 API 使用独立的本地 token。上游 API key 保存在 Windows 凭据管理器。

## 开始使用

1. 准备 Windows 10/11 x64 电脑和 ESP32-S3（16 MiB Flash、8 MiB Octal PSRAM）。
2. 安装 Agent，按[首次使用指南](docs/GETTING_STARTED.md)完成设备烧录、连接和配对。
3. 在管理页面设置 Wi-Fi 和上游模型服务。
4. 在 IDE 或其他工具中填写本地接口地址与 API key。

当前发布为 `v0.2.0-preview.1` 预览版。接口范围、客户端独立联网功能与验证情况见[验证状态](docs/VALIDATION.md)；[云端构建与附件验证记录](docs/development/2026-09-17-github-release.md)已公开。

## 开发与贡献

| 入口 | 内容 |
|---|---|
| [构建指南](docs/BUILDING.md) | 后台、Web、原生 GUI、固件及离线安装包 |
| [架构与源码导航](docs/ARCHITECTURE.md) | 模块职责与项目结构 |
| [文档中心](docs/README.md) | 使用、管理、协议和客户端集成 |
| [贡献指南](CONTRIBUTING.md) | 问题反馈与开发约定 |

## 许可

原创代码采用 [MIT](LICENSE)。第三方组件保留各自许可，见 [THIRD_PARTY.md](THIRD_PARTY.md)。
