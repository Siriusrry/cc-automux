# 脚本说明

[English](README.md) · [使用说明](../docs/usage.zh-CN.md)

这些脚本用于管理 CC AutoMux 的当前用户 macOS LaunchAgent。可以从任意目录运行，路径会按仓库位置解析。

## 脚本列表

| 脚本 | 用途 |
|---|---|
| `install.sh` | 安装现有二进制、生成 LaunchAgent 并启动服务。 |
| `start.sh` | 启动已安装的 LaunchAgent。 |
| `stop.sh` | 停止并卸载 LaunchAgent。 |
| `status.sh` | 输出 LaunchAgent 状态。 |
| `logs.sh` | 查看或跟踪 stdout/stderr 日志。 |
| `uninstall.sh` | 停止服务并移除已安装文件。 |
| `_lib.sh` | 共享实现辅助文件，不要直接运行。 |

本地 smoke test 不在此目录：`../tests/smoke/run.sh`。

## 安装

先构建二进制：

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
```

非交互安装：

```bash
./scripts/install.sh
```

可选的首次配置值：

```bash
PORT=9000 \
CLIPROXY_UPSTREAM=https://127.0.0.1:8317 \
LOG_MAX_MB=200 \
./scripts/install.sh
```

| 变量 | 默认值 | 含义 |
|---|---:|---|
| `PORT` | `8765` | 回环监听端口，主机始终为 `127.0.0.1`。 |
| `CLIPROXY_UPSTREAM` | `https://127.0.0.1:8317` | `/cpa` 上游 URL。 |
| `LOG_MAX_MB` | `100` | 每个日志文件的大小上限，单位 MB。 |
| `CC_AUTOMUX_CONFIG` | 未设置 | 可选的 `config.json` 绝对路径。 |

安装器要求 `dist/cc-automux` 已存在且可执行；它不会构建、格式化或测试源码。首次启动时，二进制会使用这些初始值生成不存在的配置文件。再次安装时，已有配置仍然优先。

安装路径：

```text
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/stdout.log
~/Library/Logs/cc-automux/stderr.log
```

安装完成后，打开 `http://127.0.0.1:<port>/admin` 配置账号、密钥和路由。如果已有配置使用了其它端口，请使用配置中的端口。

## 启动、停止与状态

```bash
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
```

LaunchAgent 标识为 `com.Siriusrry.cc-automux`。服务由 launchd 保持运行，因此应使用 `stop.sh`，不要直接杀进程。

## 日志

```bash
./scripts/logs.sh
./scripts/logs.sh --out
./scripts/logs.sh --err --last 200
./scripts/logs.sh --both --no-follow
```

| 选项 | 含义 |
|---|---|
| `--out` | 只显示 `stdout.log`。 |
| `--err` | 只显示 `stderr.log`。 |
| `--both` | 显示两个日志（默认）。 |
| `--last N` | 显示最后 `N` 行（默认 `100`）。 |
| `--no-follow` | 只输出一次，不跟踪新日志。 |
| `-h`、`--help` | 显示帮助。 |

跟踪日志时按 `Ctrl-C` 退出。正常事件写入 stdout，失败和诊断信息写入 stderr。

## 卸载

```bash
./scripts/uninstall.sh
```

使用 `--keep-logs` 保留日志目录：

```bash
./scripts/uninstall.sh --keep-logs
```

脚本会停止服务，只处理本应用的 plist、应用目录和日志目录。Finder 可用时会将它们移入废纸篓；无图形界面时会警告并永久删除相同目标。通过 `CC_AUTOMUX_CONFIG` 指向应用目录之外的配置文件不会被删除。

## Smoke test

在仓库根目录运行隔离的本地端到端测试：

```bash
GOCACHE=/tmp/cc-automux-go-cache ./tests/smoke/run.sh
```

测试会把二进制构建到临时目录，使用临时端口和配置，不访问网络，也不触碰已安装的服务。
