# Windows 离线发布

应用运行依赖均随包提供：Go 后台内嵌 Web 资源；wxWidgets 原生 GUI 静态链接；首次配对与烧录工具分别为 `tools/provision-device.exe` 和 `tools/esptool.exe`，无需用户预装 Python、Node 或 ESP-IDF。首次配对生成的私有文件不能放进公共分发包。

构建顺序为 Web、Go、原生 GUI、固件、离线工具，最后执行 `scripts/package.ps1`。该脚本从固定文件清单构建临时目录，生成内容 SHA-256 校验表、Windows x64 ZIP 和 NSIS 安装程序。`README.md`、桌面 exe、固件镜像、独立工具和许可证缺失时构建失败。

NSIS 默认使用当前用户权限安装到 `%LOCALAPPDATA%\Programs\uart2llm`，创建开始菜单入口，不配置系统服务、开机启动或防火墙规则。升级/卸载前要求关闭 GUI，并通过 CLI 停止后台。卸载保留 `%APPDATA%` 下独立存放的配置和 Windows 凭据。

构建示例（PowerShell 7）：

```powershell
./scripts/build-offline-tools.ps1 -Python python
./scripts/package.ps1 -MakeNSIS C:/Tools/NSIS/makensis.exe
```

亦可使用 WSL 中解压安装的 NSIS，完全不修改 Windows 安全策略：

```powershell
./scripts/package.ps1 -LinuxMakeNSIS /tmp/uart2llm-nsis/root/usr/bin/makensis -LinuxNSISDirectory /tmp/uart2llm-nsis/root/usr/share/nsis
```

官方 NSIS 发行版或发行系统仓库的 `nsis`/`nsis-common` 包均可使用。归档未签名；代码签名及真实硬件/长时间验收须按项目验证记录执行。

## 离线工具来源

PyInstaller 将源脚本与 CPython 及锁定 Python 依赖打包。构建时使用 `-S`，仅引入项目局部依赖目录及标准库，避免无关系统包进入发行物。`offline-tools-requirements.txt` 记录版本。所有运行时及第三方许可证位于 `licenses`。

esptool 为 GPLv2 程序。分发包包含未修改的对应源码 `tools/source/packaging/sources/esptool-5.4.0.tar.gz`、入口脚本和构建脚本。`tools/source` 可作为独立工程目录执行其 `scripts/build-offline-tools.ps1` 重建工具；首次获取构建依赖需要联网，也可先按 requirements 缓存依赖。
