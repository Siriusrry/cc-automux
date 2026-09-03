# 脚本说明

[English](README.md) · [使用说明](../docs/usage.zh-CN.md)

这些脚本用于管理当前 CC AutoMux v1 服务的当前用户 macOS LaunchAgent。脚本不实现配置逻辑，也不自行拼接 JSON。

## 脚本列表

| 脚本 | 用途 |
|---|---|
| install.sh | 安装二进制，通过 cc-automux init 初始化 v1 配置，生成 LaunchAgent 并启动。 |
| start.sh | 启动已安装的 LaunchAgent。 |
| stop.sh | 停止并卸载 LaunchAgent。 |
| status.sh | 输出 LaunchAgent 状态。 |
| uninstall.sh | 停止服务并移除已安装文件。 |
| _lib.sh | 共享实现辅助文件，不要直接运行。 |

## 安装

先构建二进制：

~~~
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
~~~

运行安装器：

~~~
./scripts/install.sh
~~~

首次安装时，脚本会询问是否自动生成高强度 management key；选择手动设置时使用二进制的可见输入提示。

安装器调用共享初始化核心，不自行写 JSON。重新安装会保留已有配置和 key。

唯一支持的环境覆盖是 CC_AUTOMUX_CONFIG，且必须是绝对路径。监听地址和 log_max_bytes 等服务配置保存在 config.json 中，通过管理 API 修改。

安装路径：

~~~
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/cc-automux.log
~/Library/Logs/cc-automux/cc-automux.log.1
~/Library/Logs/cc-automux/bootstrap.log
~~~

LaunchAgent 只携带可选的 CC_AUTOMUX_CONFIG 覆盖，不再注入旧路由或上游环境变量。配置文件路径由配置核心解析。

进程把结构化 JSON Lines 写入 `cc-automux.log`，并在 `cc-automux.log.1` 保留一代旧记录；配置中的 `log_max_bytes` 是两者的总预算。`bootstrap.log` 只接收结构化日志可用前发生的致命启动错误。

## 启动、停止与状态

~~~
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
~~~

LaunchAgent 标识为 com.Siriusrry.cc-automux，服务始终只监听 loopback。

结构化日志历史通过需要认证的 `GET /api/v1/logs` 读取，实时记录通过需要认证的 `GET /api/v1/logs/stream` 推送；不再提供命令行日志查看脚本。

## 卸载

~~~
./scripts/uninstall.sh
./scripts/uninstall.sh --keep-logs
~~~

脚本只处理本应用的明确 plist、应用目录和日志目录。通过 CC_AUTOMUX_CONFIG 指向应用目录之外的配置不会被删除。
