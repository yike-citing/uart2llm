# DeepSeek Harness 可选集成

本目录提供本地提供方的有界调度插件。它不是 Harness 本体；安装和使用 Harness 需要用户自行完成。

当前验证过 dsh 0.1.5-rc.1 / dsh-llm-pi-ai 0.1.5-rc.2。该版本 standard 模式已有 25/27 个工具通过真实模型测试，联网搜索和网页读取未通过，完整边界见[验证状态](../../docs/VALIDATION.md)。

## 配置

在 Harness 中增加 OpenAI completions 兼容提供方，名称可设为“uart2llm 本地代理”，Base URL 为 `http://127.0.0.1:8765/v1`，使用 `uart2llm.exe token api` 获得的本地 token。不要复制上游 API key。

DeepSeek 自定义地址需要适配器的供应商兼容字段，参考 `scripts/harness-configure.py`。该辅助脚本针对已验证的两个 DeepSeek 模型；更换上游应自行修改模型声明，不把这些模型当作代理内置模型。已存在同地址提供方时保留其定制和凭据。

自动化脚本通过 Harness 的正式认证接口操作。它们要求用户事先正常登录，私有 cookie 文件位于 `%LOCALAPPDATA%/uart2llm/harness-validation/cookies.txt`。源码不携带此文件，也没有获取账号权限或伪造登录的逻辑。没有该认证文件时使用 Harness 自身界面完成配置。

## 调度

Harness 四个新聊天还会产生四个标题请求，可能超过网关四路上限。`scheduler.mjs` 在 `llm/stream` 扩展点只调度提供方 ID `uart2llm`：最多四个活动请求、16 个等待、120 秒等待上限。不改变数据、不重试、不重放，保留自动标题；取消在实际发送前移出队列。

```powershell
python scripts/harness-install.py
```

安装器针对 Windows 用户级 npm 安装位置的 Harness，使用其自身 YAML 解析和原子写入组件。可执行配置会被拒绝修改，原配置先备份。安装后在没有活动任务时正常重启 Harness。插件配置引用本目录绝对路径，移动仓库后需更新配置。

## 复验

Python 需要 `websocket-client`，图片试验另需 Pillow。模型测试会产生真实上游调用；报告选择新文件名。

```powershell
node --test integrations/deepseek-harness/scheduler.test.mjs
python scripts/harness-check.py --phase parallel --seconds 180 --report .local/harness-parallel.json
python scripts/harness-check.py --phase cancel --seconds 120 --report .local/harness-cancel.json
```

其他阶段：`smoke`、`workflow`、`advanced`、`coordination`、`image`、`reasoning`。每阶段只代表其覆盖范围。试验会创建隔离会话，并更新默认模型选择；只清理自己的任务。

`harness-search-check.py` 是负向搜索边界试验：在无活动任务时临时修改搜索路由，试验后恢复，始终返回非零，不能当作搜索能力通过。

卸载时只移除 Web 插件配置中 `uart2llm-llm-scheduler` 插入项，然后在任务空闲时重启。不要用整份旧备份覆盖后来的个人设置。
