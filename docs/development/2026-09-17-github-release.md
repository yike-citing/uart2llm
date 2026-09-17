# 2026-09-17 GitHub 编译与首次公开发布

仓库：[yike-citing/uart2llm](https://github.com/yike-citing/uart2llm)。

发布：[v0.2.0-preview.1](https://github.com/yike-citing/uart2llm/releases/tag/v0.2.0-preview.1)。构建源码提交 `60939e801b3057642eadf82def01c641919a5422`；发布为 pre-release，不改变硬件全功能验收边界。

## 单 EXE 交付

- Agent 静态构建为 Windows GUI 子系统 EXE，内嵌 Web、托盘和许可证。
- 无参数启动自动启动/复用后台并打开本地管理页；命令行参数仍支持状态、token、退出和许可证等操作。
- 未安装可选 wxWidgets 界面时，“打开管理窗口”使用内置 Web，不再要求额外管理 EXE。
- 修复托盘剪贴板的整数地址转 Go 指针用法，改为通过 Windows ABI 复制到系统分配的内存，Go vet 已通过。

## GitHub Actions 实际结果

[构建运行 35175915363](https://github.com/yike-citing/uart2llm/actions/runs/35175915363)：**success**。

| 任务 | 结果 |
|---|---|
| Windows Web 构建与 72 项前端测试 | 通过 |
| Windows Go vet、全部 Go 测试包 | 通过；本机先前受应用控制阻止的组在云端完成 |
| Python 回归 | 12 项通过；1 项依赖历史本机报告的检查按预期跳过 |
| Harness 调度器回归 | 11 项通过；安装器需要外部 Harness，不在云端此任务范围 |
| 单 EXE 验证 | 版本、内嵌许可证、后台启动/复用、Web 资源、认证状态和关闭通过 |
| 独立配对/烧录工具 | 打包与启动检查通过 |
| ESP-IDF 6.0.1 GPIO UART 固件 | 编译通过 |
| ESP-IDF 6.0.1 原生 USB 验证固件 | 编译通过 |
| 发布任务 | 校验附件后公开 pre-release |

本机新 EXE 曾被应用控制策略阻止执行；未调整或绕过系统策略。云端在干净 Windows runner 中执行生命周期验证，不占用用户当前后台或 COM 设备。此轮未重新烧录实板，也不声称已经完成新的固件硬件验收。

## 发布附件复核

公开发布后下载全部 7 个附件，逐一比对 GitHub 提供的 digest 和 `SHA256SUMS`；核对 ZIP CRC、固件内部散列与版本，未包含设备配对文件。

单 EXE：`uart2llm-agent-windows-x64.exe`，7,951,360 字节，Windows GUI 子系统。

SHA-256：`7d56027e8d0c85632cc1560a5e3fe9ebdb9be370563b1e5ad788d421abcb4956`。

设备工具与 UART/USB 固件独立下载；单 EXE 是日常运行入口，首次设备准备仍按发行说明操作。项目保持未签名预览版，不承诺安全软件免告警。
