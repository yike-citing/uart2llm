# 第三方组件与许可

项目 MIT 许可仅适用于原创代码。保留的第三方通知位于 `packaging/licenses`，依赖版本与来源以以下清单为准：

| 组件 | 来源与构建入口 |
|---|---|
| Go 工具链与运行时 | `go.mod`、`packaging/licenses/Go-LICENSE.txt` |
| React / React DOM 与 Web 构建依赖 | `web/package.json`、`web/package-lock.json` |
| wxWidgets / C++ 工具链 | `native/CMakeLists.txt`、`native/THIRD_PARTY.md` |
| ESP-IDF、FreeRTOS、TLS 与 Wi-Fi 组件 | `firmware/main/idf_component.yml`、`firmware/dependencies.lock`、ESP-IDF 分发通知 |
| Python 和离线工具依赖 | `packaging/offline-tools-requirements.txt`、`packaging/licenses` |
| esptool 5.4.0 | 对应源码 `packaging/sources/esptool-5.4.0.tar.gz`，独立入口 `packaging/esptool_entry.py` |

esptool 按其 GPLv2 许可分发，未将其代码改为 MIT。Windows 离线包将其作为独立进程工具，并携带对应源码、构建脚本和许可证。构建或分发修改版时应同时保留适用的源码与通知；具体以组件自己的许可文本为准。

DeepSeek Harness、OpenCode 是可选外部测试客户端，不随源码副本分发其应用、账号或会话。本项目的 Harness 调度适配器使用扩展接口独立实现。管理界面参考 sub2api 的交互组织，未复制其源码、品牌或图像；设计说明见 `docs/implementation/product-ui-design.md`。

本目录的通知覆盖此前 Windows 构建使用的组件；重新选择依赖或工具链后，应核对实际发行物的通知，不能把静态清单当作任意新构建的自动许可证审计。
