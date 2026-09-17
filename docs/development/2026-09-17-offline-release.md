# 2026-09-17 完整离线 Release

## 交付目标

从 GitHub Release 下载 `uart2llm-windows-x64-offline.zip` 后，即可取得 Windows 单文件 Agent、GPIO UART 固件、原生 USB 验证固件、配对工具、烧录工具和说明，无需克隆仓库或安装开发环境。保留各组件单独下载，方便只更新 Agent。

## 实现与本地验证

- 发布工作流等待 Windows 与两种 ESP32-S3 固件构建通过，再将产物汇总为完整离线包。
- 固件分目录存放，保留原始分区布局、烧录参数、内部散列与许可；设备工具附带许可和对应源码。
- 包内生成逐文件 SHA-256，Release 另附各下载附件的 SHA-256。
- 首次使用说明包含下载选择、目录布局、配对、烧录、连接、配置和客户端接入，不依赖仓库内的相对链接。
- 4 项打包回归通过：完整文件和散列、缺失工具拒绝、固件损坏拒绝、不安全路径和私有配对文件拒绝。
- 使用上一版真实发布附件试打包成功，180 个文件。该检查仅验证打包路径，新版仍由云端重新编译。
- 公开源码扫描通过，无本机配置或设备凭据加入发布。

## 云端结果

[Release v0.2.0-preview.2](https://github.com/yike-citing/uart2llm/releases/tag/v0.2.0-preview.2) 已公开，源码提交 `44fbe9ebbb074ecad5998a7045bac0f1d3561a9e`。

[GitHub Actions 35181242033](https://github.com/yike-citing/uart2llm/actions/runs/35181242033) 全部成功：

| 检查 | 实际结果 |
|---|---|
| Windows Web 构建与测试、Go vet 与测试 | 通过 |
| Python 与 Harness 调度器回归 | 通过 |
| 单 EXE 版本、内嵌页面、后台启动/复用/关闭 | 通过 |
| 独立配对与烧录工具构建、启动 | 通过 |
| ESP-IDF UART 固件与 USB 验证固件编译 | 均通过 |
| 完整离线包生成、Release 附件发布 | 通过 |

## 公开附件下载复核

从已公开的 Release 重新下载全部 8 个附件，核对 GitHub digest、发布 `SHA256SUMS`、所有 ZIP CRC、固件内部散列与版本、Agent Windows GUI 子系统与嵌入版本，全部通过。

完整包内含 180 个文件，逐项通过内部 SHA-256 校验；EXE、说明、许可证和各组件内容均与独立附件逐字节一致。未包含预生成设备配对凭据。

| 下载项 | 字节数 | SHA-256 |
|---|---:|---|
| `uart2llm-windows-x64-offline.zip` | 35,707,898 | `8b3e5276bb38e1876887989959004b5c54ffa2d9319d66783979e77a8dabc895` |
| `uart2llm-agent-windows-x64.exe` | 7,951,360 | `9436ab2fe74b9b6d3ae41bf565a1dda7c8fc525597c5d5dd6be1037dd0f7287a` |

本轮没有连接设备重新烧录或运行模型负载；验证范围为云端编译、自动化回归、单 EXE 生命周期和发布包完整性，不改变先前硬件与模型验收结论。

此次发布改进分发方式，不增加模型接口或硬件验收结论；保留预览版标记。
