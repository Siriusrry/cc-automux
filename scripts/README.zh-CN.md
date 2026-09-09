# 脚本说明

[English](README.md) · [使用说明](../docs/usage.zh-CN.md)

这些脚本把 CC AutoMux 安装为 macOS 与 Linux 上的当前用户服务。脚本在运行时判断平台并分派给该平台自身的服务管理器：macOS 使用 LaunchAgent，Linux 使用 `systemd --user` unit。注册始终属于当前用户，不请求提权。脚本不实现配置逻辑，也不自行拼接 JSON。

## 脚本列表

| 脚本 | 用途 |
|---|---|
| install.sh | 安装二进制，通过 cc-automux init 初始化 v1 配置，注册当前用户服务并启动。 |
| start.sh | 启动已安装的服务。 |
| stop.sh | 停止服务。 |
| status.sh | 输出服务状态。 |
| uninstall.sh | 停止服务、移除自启注册并删除已安装文件。 |
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

唯一支持的环境覆盖是 CC_AUTOMUX_CONFIG，且必须是绝对路径。监听地址和 log_max_bytes 等服务配置保存在 config.json 中，通过 `/management` Web 控制台或管理 API 修改。

macOS 安装路径：

~~~
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/cc-automux.log
~/Library/Logs/cc-automux/cc-automux.log.1
~/Library/Logs/cc-automux/bootstrap.log
~~~

Linux 安装路径，设置了 XDG_CONFIG_HOME 与 XDG_STATE_HOME 时按其取值：

~~~
~/.config/cc-automux/bin/cc-automux
~/.config/cc-automux/config.json
~/.config/systemd/user/cc-automux.service
~/.local/state/cc-automux/cc-automux.log
~/.local/state/cc-automux/cc-automux.log.1
~~~

服务注册只携带可选的 CC_AUTOMUX_CONFIG 覆盖，不注入旧路由或上游环境变量。配置文件路径由配置核心解析。

进程把结构化 JSON Lines 写入 `cc-automux.log`，并在 `cc-automux.log.1` 保留一代旧记录；配置中的 `log_max_bytes` 是两者的总预算。

结构化日志可用之前的致命启动错误写入 stderr，两个平台的收集方式不同：

- **macOS：** 进入 `~/Library/Logs/cc-automux/bootstrap.log`。安装器每次运行都会截断该文件——它只承载需要人介入的错误，上一轮的内容对当前排查没有价值。
- **Linux：** 进入 journal，用 `journalctl --user -u cc-automux.service` 查看。journald 自身有系统级容量上限和轮转策略，因此 unit 不指定任何日志文件路径。

## 启动、停止与状态

~~~
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
~~~

服务在 macOS 上名为 com.Siriusrry.cc-automux，在 Linux 上名为 cc-automux.service；两个平台都始终只监听 loopback。

结构化日志历史通过需要认证的 `GET /api/v1/logs` 读取，实时记录通过需要认证的 `GET /api/v1/logs/stream` 推送，单条完整记录通过 `GET /api/v1/logs/record` 读取；不提供命令行日志查看脚本。`GET /api/v1/status` 会报告进程当前是否还能写入自己的日志。

## 卸载

~~~
./scripts/uninstall.sh
./scripts/uninstall.sh --keep-logs
~~~

脚本只处理本应用自己的注册文件、应用目录和日志目录，共享的上级目录保持不动。通过 CC_AUTOMUX_CONFIG 指向应用目录之外的配置不会被删除。

macOS 上被移除的内容进入废纸篓，可通过 Finder 的“放回原处”恢复；Linux 上是永久删除，因为该平台对任意路径没有等价的用户级废纸篓保证。
