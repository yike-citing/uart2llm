# ESP32-S3 固件

目标为 ESP32-S3、16 MiB Flash、8 MiB **Octal PSRAM**。项目使用 ESP-IDF **6.0.1** 和锁定的 `espressif/cjson 1.7.19`。不包含通用代理、API key、模型 JSON 解析或 SPI 驱动。

## 构建、烧录、配对

在 ESP-IDF 6.0.1 PowerShell 环境执行：

```powershell
idf.py -C firmware set-target esp32s3
idf.py -C firmware menuconfig
idf.py -C firmware build
python scripts/provision-device.py --output <私有设备目录>
idf.py -C firmware -p COMx flash
python -m esptool --chip esp32s3 --port COMx write-flash 0x12000 <私有设备目录>/pairing.bin
```

也可直接运行发行包中的 `provision-device.exe --output <私有设备目录>`，不需要 Python 或 ESP-IDF。源码形式仅依赖 Python 和 `esp-idf-nvs-partition-gen`；`--idf-path` 可选，用于对照官方源码的 SRP 参数。

`pairing.json` 仅含本设备随机生成的 `username/password`，交给电脑配对并保存到凭据存储。`pairing.bin` 仅含 SRP SHA-512 盐与验证值，不含密码。生成器拒绝覆盖现有凭据；Windows 上移除配对文件继承 ACL 并限定当前用户读写。程序不打印凭据。测试样本 `.tools/provision-test/` 不能作为正式设备凭据分发。

板级菜单 `uart2llm board wiring` 定义 UART 控制器和 TX/RX/RTS/CTS。默认 UART1 TX17/RX18，RTS/CTS 不连接。控制台使用独立 USB Serial/JTAG，USB 控制台引脚不能与隧道冲突；Flash/Octal PSRAM 专用引脚、缺失引脚、引脚重叠在编译时拒绝。以后 SPI 引脚也在同一菜单，但当前固件明确返回 `spi:false`。

每次启动使用固定 `115200/8N1/无流控`，即使保存的期望速率不同，以确保恢复入口可用。启动后 `desired/persisted` 保留用户配置，`effective` 报告实际启动链路参数。PSRAM 未初始化或少于 8 MiB 时不启动代理。Flash 分区包含双 6 MiB OTA 槽、普通 NVS、独立配对 NVS、OTA 元数据和 coredump。

## 独立 USB 实板验证构建

`CONFIG_U2_USB_VALIDATION` 默认关闭。只有 USB 连接、没有外接业务 UART 时，可以使用 `sdkconfig.usb-validation` 生成独立验证固件；生产 `sdkconfig.defaults` 仍使用 GPIO UART。验证构建复用完整八通道协议、Security 2 和网络逻辑，只替换底层字节读写为 ESP-IDF `esp_driver_usb_serial_jtag`。

在 ESP-IDF PowerShell 中构建：

```powershell
$fwDir = (Resolve-Path firmware).Path
idf.py -C $fwDir -B "$fwDir/build-usb" -D "SDKCONFIG=$fwDir/build-usb/sdkconfig" -D "SDKCONFIG_DEFAULTS=$fwDir/sdkconfig.usb-validation" build
```

使用 `build-usb` 中配套 bootloader、分区表、OTA 初始数据和应用镜像。该构建显式关闭主控制台、次控制台及启动日志，panic 静默重启；编译期检查禁止 USB 控制台与二进制通信混用。ROM 复位早期可能产生的字节由协议分帧和重试处理。USB 发送与接收都有有界等待，不会因电脑停止读取而永久锁住管理维护任务。

能力报告 `transport:"usb_serial_jtag_validation"`、`uart_configuration_supported:false`，状态中 `uart.active:false`。UART 配置在配置目录中标记只读/不支持，显式下发 UART 参数返回业务错误；CDC 主机侧波特率没有物理 UART 速率含义。编译后的 UART GPIO 默认值仍公开，但 USB 固件不会初始化业务 UART 或配置这些 GPIO。**USB 实板测试不能替代 GPIO UART 波特率、线路或 RTS/CTS 验收。**

## 串口与网络

完整字节格式见 `protocol.md`。八个独立逻辑通道：0 管理，1–4 TCP，5 遥测，6 日志，7 OTA。每通道独立序号、ACK 和有界队列；管理请求处理器轮询 0/5/6/7，回复返回原通道。Security 2 配对仅使用通道 0，主机加密 RPC 全局串行，跨通道仍严格保持加密 nonce 顺序。

DATA 最大 1024 字节，控制帧最多 4096 字节。每通道停止等待 ACK、去重、CRC32/COBS 校验、500 ms 重传、10 次无响应重试；BUSY 表示接收队列已满并重置无响应重试预算，不把慢消费者当成丢线。控制/数据队列使用有界 PSRAM；UART 驱动使用内部合适内存。每数据通道发送四帧，接收四个 DATA 位置并另保留一个 CLOSE 位置；管理各八帧，遥测/OTA 各四帧，日志各两帧。管理发送优先级高于数据，日志优先级较低。日志读取最多每秒两次，过快轮询返回加密的业务错误，不破坏安全会话；内部事件仍正常写入有界日志环。UART 参数切换必须等待全部四个管理类通道回复确认。发送采用有截止时间的 UART FIFO 写入，缺失 CTS 不会锁住回滚功能。

四路 socket 分别有发送/接收任务和有界 DNS/连接任务，防止全双工反压互锁，慢 DNS 不妨碍取消。连接代次防止关闭旧 socket 的工作误关闭复用后的新连接。ABORT 控制帧以 OPEN 序号限定当前连接，在可靠 DATA 受反压时先通知取消并排空；旧 ABORT 不会影响复用后的连接。可靠 CLOSE 位于先前 DATA 后面；复用通道前完成双向关闭屏障。串口新会话清除鉴权、TCP 和旧帧，绝不续接或重放旧上游请求。

只有成功完成 Security 2 并调用加密 RPC 后才允许 TCP OPEN。电脑首先调用 `target.set`，设备只允许精确的已授权 `host:port`；授权仅对当前串口会话有效。DNS 和 TCP 连接都在设备完成。Wi-Fi 掉线关闭数据通道并保留串口管理入口。

## 管理 RPC

明文端点 `sec-session` 使用官方 Protocomm Security 2 (SRP6a-3072/SHA-512、AES-256-GCM patch 1)。`rpc` 端点由 **ESP-IDF protocomm_req_handle** 完成鉴权及加解密，无自制替代算法。

解密请求为 `{id,method,params}`，成功响应 `{id,result}`，业务失败 `{id,error:{code:"device_error",message}}`。响应 JSON 限制 3800 字节，超过时明确返回协议错误而非截断。所有操作要求配对后的管理会话。

| 方法 | 参数 / 结果 |
|---|---|
| `capabilities` | 固件、芯片、连接数、OTA 分片大小、安全版本及构建 GPIO；SPI 明确不支持 |
| `target.set` | `{host,port}`；设置本会话唯一允许连接的目标 |
| `config.schema` | 配置数组，含 key/type/min/max/default/secret/apply/persistence/scope/depends_on |
| `config.get` | revision、desired、effective、persisted、pending、confirm_deadline_ms、current_time_ms、session |
| `config.stage` | `{values:{"wifi.ssid":"…",…}}`，校验并合并已知字段 |
| `config.apply` | 暂存候选配置、应用，开始 30 秒确认窗口 |
| `config.confirm` | UART 切换完成后，单个 NVS blob 原子提交配置和 revision |
| `config.rollback` | 先回复，至少一秒且回复被 ACK 后回退，UART 变为 115200/无流控 |
| `config.defaults` | 仅将默认值放入 desired，不应用 |
| `config.reset` | 默认值 + apply；保留配对凭据，需要显式 confirm |
| `state.get` | 按 telemetry.interval_ms 缓存的状态快照，含实际采样时刻；配置待确认或缓存属于旧会话时即时采样 |
| `state.schema` | `{offset:0,limit:12}`，返回 `{items,next_offset}`，最后 next_offset=null |
| `tasks.get` | `{after_id:0}`，最多 12 个任务与 next_after_id，末页为 null；兼容旧 offset。按递增编号读取实时快照，避免任务退出引起位置偏移；不是跨页原子历史快照。 |
| `logs.get` | `{after:0}`，最多 16 项；最多保留 32 项，不包含请求体、密码或 API key |
| `diagnostics` | `{action:"health"/"snapshot"/"network"/"wifi_scan"/"wifi_reconnect"}`，未知动作明确拒绝；wifi_reconnect 终止旧 TCP 并重连 Wi-Fi，不改变配置 |
| `device.restart` | 回复后延迟重启 |
| `device.self_test` | 验证内存完整性，确认 OTA 候选固件有效 |
| `ota.begin` | `{size,sha256?}`，SHA-256 为 64 位小写十六进制 |
| `ota.write` | `{offset,data}`，data 为 base64，每片解码后最多 1024 字节，offset 必须连续 |
| `ota.end` | `{sha256}`；若 begin 未给 hash 则这里必填；两处给出时必须一致 |
| `ota.abort` | 取消当前传输 |
| `crash.read` | `{offset:0}`，base64 分片、总 size、eof，最多 1024 原始字节 |
| `crash.clear` | 清除已保存 coredump |

运行配置字段：`wifi.ssid/password/hostname`、`ip.dhcp/address/gateway/netmask/dns`、`uart.baud/flow_control`、`tcp.connect_timeout_ms/idle_timeout_ms`、`telemetry.interval_ms`、`logs.level`。密码读取返回布尔“已设置”，不返回明文。主机不可将这个布尔标记作为密码重新写入。固件不保存上游 API key。

配置切换在已发送配置回复并收到 ACK 后才执行；波特率/流控切换至少等待一秒。修改未显式涉及 UART 时，暂存的 UART 值沿用当前有效值，避免启动后的 Wi-Fi 修改意外切换到历史波特率。网络参数未改变时不会重启 Wi-Fi。SSID 非空但未获得 IP 地址时不能确认配置。确认超时、配置应用失败或重启均保留最后确认配置，链路恢复为 115200/无流控。候选配置存放另一 NVS 键，启动丢弃；确认时用单个 blob 同时保存配置和 revision，避免半套参数持久化。更换 Wi-Fi 会终止已有 TCP，不能恢复已发出请求。

## 遥测、升级与实际验证边界

状态覆盖设备 MAC、固件/构建配置、复位原因、启动时间、当前会话与鉴权、Wi-Fi/地址/DNS/RSSI/断线原因、四路连接目标和字节统计、内部 RAM/PSRAM/最低内存/最大块、任务数、帧/CRC/重试/队列水位、NVS 使用、候选 OTA/当前分区、coredump。无线信息不可用时返回 null 或明确有效性条件。未采样温度明确标记原因，不制造读数。SDK 私有内部状态及未启用功能不作为“可配置参数”暴露。

OTA 边传边写备用槽，同时计算 SHA-256；完成后先验证 digest，再调用 ESP-IDF 镜像校验及 boot 分区切换。镜像未完整、校验失败、会话变化或 60 秒传输无进展都不更换启动槽。重启后的候选固件必须在 120 秒内收到已认证主机 `device.self_test`，否则请求回滚。镜像 SHA-256/ESP-IDF 格式校验并非发行者签名；本阶段通过已配对的管理会话授权升级，未替用户烧写 Secure Boot 或 Flash Encryption 的不可逆 eFuse。

构建和验证范围见 [构建指南](../BUILDING.md) 与 [验证状态](../VALIDATION.md)。GPIO UART 与原生 USB 验证固件必须分别构建和验收。
