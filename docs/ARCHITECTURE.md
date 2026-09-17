# 架构与项目结构

## 数据路径

本地 API → Go HTTP 网关 → 电脑 TLS → 串口多路复用 → ESP32 DNS/TCP → Wi-Fi 上游。电脑 TLS 使用串口提供的 `net.Conn`，没有自动改用电脑网卡的回退。设备仅看到加密模型字节流，不解析 prompt、工具参数或 SSE。

管理路径：Web / GUI / CLI / 托盘 → 独立回环管理端口 → 设备管理通道。管理、遥测、日志、OTA 与四条 TCP 通道分别排队；端到端慢消费者通过有界队列触发背压。Security 2 握手和加密 RPC 有独立状态机；串口会话变化使旧连接失效。

## 源码导航

| 路径 | 职责 |
|---|---|
| `cmd/uart2llm` | 进程生命周期、CLI、后台和托盘入口 |
| `internal/gateway` | API 家族白名单、HTTP 转发、流式与统计 |
| `internal/link` | 分帧、确认、去重、流控、多路连接与取消 |
| `internal/serialport` | Windows COM 实现及平台边界 |
| `internal/security` | Protocomm Security 2 主机实现及公开合成测试向量 |
| `internal/device`、`internal/admin` | 设备管理与本地管理 API |
| `internal/config`、`internal/credentials` | 配置持久化与 Windows 凭据存储 |
| `internal/ui`、`internal/tray` | Web 资源嵌入和托盘展示 |
| `firmware/main` | ESP-IDF UART/USB、Wi-Fi/TCP、管理与维护 |
| `web/src` | 用户配置表单、恢复逻辑、图形看板 |
| `native/src` | 原生控件界面，调用同一后台接口 |
| `integrations/deepseek-harness` | 可选客户端调度；不改变网关容量约束 |
| `cmd/test-upstream`、`cmd/link-*` | 可控上游和传输验收工具 |

## 整理原则

保留 Go 包、固件组件和相对构建路径，避免为目录美观破坏模块边界。使用文档从历史实施记录中独立出来；原始本机报告、会话、凭据、构建缓存和二进制不进入源码副本。第三方通知和对应源码放在 `packaging`，不与原创许可混合。

构建生成的 `internal/ui/assets`、`web/dist`、`firmware/build*`、`native/build` 和 `dist` 均为可再生输出。源码只保留 Web 嵌入占位页，正式后台构建先运行 Web 构建。

## 硬边界

- 首阶段 Windows、ESP32-S3、最多四条上游请求；SPI 只保留接口与构建字段。
- TLS 和上游凭据归电脑，Wi-Fi 凭据归设备配置；日志默认不记录正文和秘密。
- `/jobs` 没有别名，采用 `/v1/fine_tuning/jobs`。
- 流中途失败终止连接，不伪造正常结束事件；写操作不自动重试。
- DeepSeek Harness 的搜索和网页读取不在四类模型 API 内，目前不支持电脑离线经 ESP32 执行这两项。

更细的字节格式见[协议](implementation/protocol.md)，配置事务与状态目录见[管理接口](implementation/management-api.md)。
