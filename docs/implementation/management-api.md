# 管理接口契约

默认 `http://localhost:8766/admin/v1`。只绑定回环，验证 Host、浏览器 Origin 和 Fetch Metadata。Web 自动建立 HttpOnly/SameSite Strict 会话，用户无需密码或 token；CLI/原生客户端仍兼容 `Authorization: Bearer <admin-token>`。API 端口使用独立 token。Web、GUI、CLI 共用本接口。

## 端点

| 方法与路径 | 作用 |
|---|---|
| POST /session、DELETE /session | POST `application/json` 空对象 `{}` 自动建立免密码浏览器会话，DELETE 清除；8 小时到期，网页自动续建。非 JSON 申请返回 415，跨站请求返回 403 |
| GET /state、GET /events | 状态快照；SSE 每秒 `event: state`，变化独立 `event: change`，最多 16 订阅者 |
| GET /ports、GET /capabilities | Windows 串口列表与主机/设备能力 |
| GET /config/schema、GET /state/schema | 所有公开配置定义与状态目录，后者汇总设备分页 |
| GET /config | 主机设置、设备 desired/effective/persisted、机密设置状态、实际监听器及 restart_required |
| PATCH /config | `{"host":{...}}` 或 `{"device":{"wifi.ssid":"..."}}`，不能跨主机与设备事务 |
| POST /config/confirm、POST /config/rollback | 确认或回滚设备候选配置 |
| POST /connect、POST /disconnect | `{port,baud,flow_control}` 连接，或停止连接及自动重连 |
| POST /pair | `{username,password}` 完成 Security 2；通过后存入 Windows 凭据存储 |
| POST /credentials | `{upstream_key}` 写入机密，不回显 |
| GET /api-token | 管理员主动请求时读取本地 API token；不返回上游 key |
| GET /logs?after=N | 主机最近 256 条 + 设备游标分页（每次最多 16 条，每秒最多 2 次） |
| GET /tasks | 汇总设备任务状态、优先级、栈余量 |
| POST /diagnostics | `{action:health\|network\|wifi_scan\|snapshot\|export\|crash_export}` |
| POST /device/action | `{action:reboot\|factory_reset\|clear_crash}`；恢复默认保留配对身份 |
| POST /firmware | application/octet-stream、Content-Length 1..4194304；流式 OTA |
| POST /shutdown | 显式停止独立后台 |
| GET /openapi.json | OpenAPI 3.0.3 描述 |

`crash_export` 返回 `{size,encoding:"base64",data}`，最大 64 KiB 诊断分区；不是任意文件缓存。固件原始 RPC 方法见 firmware.md。所有读取都明确呈现断开/未配对状态，不生成假遥测。

## 配置与恢复

设备事务：校验并暂存 → apply → 30 秒 confirm；确认失败或超时回退。UART 双端切换期间，后台暂停普通设备 RPC；确认前暂停新代理请求。`config.confirm` 同时保存本机已确认串口速率，自动重连先用该速率，再尝试固定 115200/无流控。设备每次启动都用固定恢复速率。

设备配置快照（`GET /config` 中的设备对象，以及 stage/apply 返回的设备快照）包含 `current_time_ms`、`session` 与 `confirm_deadline_ms`。`current_time_ms` 在每次固件配置回复时现算，是 ESP32 开机后的单调毫秒；`session` 是当前串口会话。待确认时的剩余时长为 `max(0, confirm_deadline_ms - current_time_ms)`，界面将该时长锚定于收到该回复时的浏览器单调时钟，再递减。不能与 `Date.now()` 或遥测缓存里的 `sample_time_ms` 直接混算；连接/会话变化后重新读取配置。若旧固件缺少当前时钟，显示剩余时长未知，不制造精确倒计时。传输耗时使显示只是提示，最终是否可确认由设备事务期限判断。

`telemetry.interval_ms` 最长可为 60 秒；配置处于 `pending:true` 的 30 秒窗口内，固件 `state.get` 每次即时采样，避免把应用前的 Wi-Fi 已连接状态用于当前设置的确认提示。非待确认时仍使用配置的采样间隔，但缓存会话与当前 HELLO 不一致时即时采样。硬件 `config.confirm` 的实时联网检查、原子持久化和超时回滚规则不变。

`desired/effective/persisted` 在设备未确认时不同；机密只显示是否设置。GPIO 是构建配置，不能通过运行 PATCH 改变。主机监听地址为重启生效，超时/上游参数作用于后续请求。`runtime.json` 保存当前监听地址供 CLI 使用，停止后删除；不包含机密。

设备配置、连接、诊断操作和升级通过统一修改锁串行化，冲突立即 409。设备配置/升级要求当前没有活动代理请求。上传不设置整次总时长，但每片设备 RPC 20 秒、等待本地输入 60 秒。逻辑通道为管理 0、TCP 1–4、遥测 5、日志 6、升级 7，各自有界队列；Security 2 的加密消息计数仍统一串行，避免 nonce 次序失配。后台每秒遥测；升级期间跳过设备遥测，以免占用升级带宽。通过串口恢复后重新取完整快照。连接、配对、Wi-Fi 和配置版本变化另存最近 256 条事件，快照的 recent_changes 可供离线诊断；SSE 使用事件 ID 和 Last-Event-ID 接续这个有界窗口。

配置类型和上下界由 `/config/schema` 提供。状态字段单位、来源、有效条件由 `/state/schema` 提供；`sampled_at` 是主机快照时间，设备报告自己的实际采样时间。不采集的硬件数据使用 null 和原因，不用零冒充。

## 错误与边界

管理错误形如 `{"error":{"message":"...","type":"proxy_error","code":"management_error",...}}`，400 表示参数或设备业务失败，401/403 为认证/来源拒绝，409 为事务冲突。请求 JSON 上限 64 KiB，诊断只允许已定义动作。串口消息、日志、队列和订阅数量都有限额。

API 代理在进入上游前检查路由、token 和容量；第五路立即 503、带 Retry-After。生成/上传/任务创建不重放。SSE 中途失败关闭流并记一条不包含正文的错误；不补造 `[DONE]`。上游状态码、端到端头、压缩字节和事件内容透传，逐跳 HTTP 头由 Go HTTP 实现正确处理。TLS 证书按 Windows 信任根验证。

后台只创建回环监听器和 UART；唯一的上游 Dialer 是串口隧道。模拟测试的 TCP Dialer 不暴露为运行时选项。
