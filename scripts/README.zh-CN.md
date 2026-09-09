# 脚本说明

[English](README.md) · [使用说明](../docs/usage.zh-CN.md)

CC AutoMux 在 macOS 使用当前用户的 LaunchAgent，在 Linux 使用 `systemd --user`。安装启用用户登录自启，不请求系统提权。macOS 需要已登录的图形会话；Linux 需要可用的 systemd 用户服务管理器。提供 amd64、arm64 二进制，macOS 最低版本为 12。

## 安装与升级

```bash
curl -fsSL https://raw.githubusercontent.com/Siriusrry/cc-automux/main/scripts/install.sh | bash
```

同一命令安装或升级到最新完整公开稳定 Release。需要 Bash、curl、tar，以及 `sha256sum` 或 `shasum`；无需源码、Go 或 Node.js。每次下载固定一个版本，并在安装前校验 SHA-256、平台和二进制版本。

首次安装选择监听端口（默认 `8765`），再选择自动生成 management key 或可见地手动输入。交互从控制终端读取，适用于 `curl … | bash`；首次安装没有控制终端时会说明原因并退出。配置由二进制初始化，shell 不拼接 JSON。

升级由目标二进制检查已有配置并予以保留，包括端口、密钥、Providers、Profiles、Auto Mode 和 Claude Code 路径；不激活 Profile，也不修改 Claude Code 配置。配置非法、版本不支持或存在未完成配置重启时，在替换前停止。下载与预检完成后保留旧程序及服务注册；替换或就绪检查失败时恢复旧安装并报告失败。若恢复也失败，会显示保留下来的恢复目录。

只有认证后的服务状态与预期产品、版本、监听地址一致，并且控制台可访问时才报告成功。输出包含实际 URL、配置路径、版本与已安装运维脚本路径。已是最新且安装完整时保持原服务运行；服务异常会单独报错，不因重复执行安装命令而无故重启。

安装与运维操作互斥。若中断后留下 `~/.cc-automux-install.lock`，先确认没有安装或运维脚本仍在运行，处理报错中的恢复状态，再删除这个空锁目录后重试。

## 本地开发版

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh --local
```

构建需要 Go 1.22 或更新版本。`--local` 只读取脚本所在项目的 `dist/cc-automux`，不联网、不自动构建，复用已安装服务和配置。同版本也安装本次选定产物；试用前自行备份需要保留的程序与配置。

不带 `--local` 时始终获取公开 Release，不会因为当前目录有 `dist/` 而切换来源。从本地版升级到公开版，须同时满足语义版本更高和配置受目标版本支持：`v1.1.0-dev → v1.1.0` 是向前升级，`v1.1.0-dev → v1.0.1` 是降级。同版本覆盖本地构建和公开降级均会被拒绝；需要切回时自行处理程序与配置。本地安装缺失二进制时也不能借修复流程绕过版本判断。

## 安装位置

| 内容 | macOS | Linux 默认路径 |
|---|---|---|
| 应用目录 | `~/Library/Application Support/cc-automux/` | `~/.config/cc-automux/` |
| 二进制 | 应用目录 + `bin/cc-automux` | 应用目录 + `bin/cc-automux` |
| 配置 | 应用目录 + `config.json` | 应用目录 + `config.json` |
| 运维脚本 | 应用目录 + `scripts/` | 应用目录 + `scripts/` |
| 服务注册 | `~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist` | `~/.config/systemd/user/cc-automux.service` |
| 日志目录 | `~/Library/Logs/cc-automux/` | `~/.local/state/cc-automux/` |

Linux 的 `XDG_CONFIG_HOME` 决定应用和服务注册的根目录，`XDG_STATE_HOME` 决定日志根目录。两平台都可通过 `CC_AUTOMUX_CONFIG` 指定配置的绝对路径。升级优先读取已有服务注册和安装路径记录，不因终端环境变化另建默认配置。服务注册固定配置路径和 Linux 日志路径，供后续启动沿用。

应用目录还保存用户说明、服务模板、`install-paths` 路径记录，以及值为 `public` 或 `local` 的 `install-source` 来源记录。这些安装元数据独立于产品配置；已安装运维脚本不依赖下载解压目录。

结构化日志文件为 `cc-automux.log` 和 `cc-automux.log.1`，在控制台设置两者的总容量。日志尚不可用时的启动错误在 macOS 写入 `bootstrap.log`（替换安装时截断）；Linux 使用用户服务 journal：

```bash
journalctl --user -u cc-automux.service
```

## 启动、停止与状态

使用安装器显示的实际应用目录。macOS 默认安装示例：

```bash
APP_DIR="$HOME/Library/Application Support/cc-automux"
"$APP_DIR/scripts/status.sh"
"$APP_DIR/scripts/stop.sh"
"$APP_DIR/scripts/start.sh"
```

Linux 默认安装先设置 `APP_DIR="$HOME/.config/cc-automux"`，再运行同样命令。`start.sh` 检查真实就绪状态；`status.sh` 显示服务管理器状态及认证后的网关就绪结果。请求历史和实时日志在控制台 **Logs** 页面查看。

## 卸载

若 Claude Code 不再使用此网关，请先修改其连接设置，再运行已安装脚本：

```bash
"$APP_DIR/scripts/uninstall.sh"
# 或保留应用日志目录：
"$APP_DIR/scripts/uninstall.sh" --keep-logs
```

卸载会停止服务、取消登录自启并删除应用目录，其中包含默认配置。默认同时删除日志；`--keep-logs` 保留日志。应用目录之外的自定义配置保留。不会自动还原或删除 Claude Code 的 `settings.json` 及其备份。

macOS 优先通过 Finder 移入废纸篓，无图形会话时回退为永久删除；Linux 为永久删除。共享的上级目录保持不动。

## 脚本职责

| 脚本 | 用途 |
|---|---|
| `install.sh` | 公开下载入口，或通过 `--local` 显式安装本地产物。 |
| `start.sh`、`stop.sh`、`status.sh` | 操作已安装用户服务并检查状态。 |
| `uninstall.sh` | 删除安装，可选择保留日志。 |
| `_install.sh`、`_lib.sh` | 共享实现，使用上方入口即可。 |
| `release.py` | 构建、检查各平台发布包及校验清单；需要 Python 3.11+ 和 Go。 |
