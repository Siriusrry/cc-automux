# 使用说明

[English](usage.md) · [项目介绍](../README.zh-CN.md)

## 使用条件

- 安装脚本适用于 macOS 与 Linux：在 macOS 上注册当前用户 LaunchAgent，在 Linux 上注册当前用户 systemd unit。
- 从源码构建需要 Go 1.22 或更高版本。
- 需要 AnyRouter 账号、正在运行的 CLIProxyAPI，或其它已配置的分类器目标。

## 构建与运行

构建二进制文件：

```bash
go test ./...
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
```

前台运行：

```bash
./dist/cc-automux
```

也可以把已构建的二进制安装为当前用户的服务：macOS 上是 LaunchAgent，Linux 上是 `systemd --user` unit：

```bash
./scripts/install.sh
```

首次安装时，安装器会询问是否自动生成 management key；选择否则在二进制的可见提示中手动输入。配置文件由 `cc-automux init` 写入，脚本自身从不拼接配置。重新安装会原样保留现有 `config.json` 及其中的密钥。唯一支持的环境覆盖是 `CC_AUTOMUX_CONFIG`，取值必须是配置文件的绝对路径。

## 配置网关

打开本地配置台：

```bash
open http://127.0.0.1:8765/admin
```

只需配置实际使用的路由：

- **AnyRouter：**有序入口 URL，以及零个或多个账号标签和密钥。新会话会分配到可用账号，并在同一会话内保持粘性。
- **CPA：**CLIProxyAPI 上游 URL、一个可选密钥和一个可选 CA 文件。
- **分类器拦截：**通常保持各路由默认目标；也可设置统一目标及其 URL、密钥、平台类型、模型覆盖和 TLS 选项。
- **服务：**Active/Pass-through 模式、回环端口和日志大小上限；该上限由活动日志文件和它的一代归档文件共同占用。

大多数保存会立即生效。修改端口或日志上限时，进程会自动原地重启。直接编辑 `config.json` 不会被监测，手动修改后需要重启服务。

Pass-through 模式保留前缀路由和 AnyRouter 入口故障切换，但禁用网关托管凭据、账号轮询和兼容性改写。

## 连接 Claude Code

每个 Claude Code 进程选择一个 Base URL：

```bash
# AnyRouter
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/any \
ANTHROPIC_AUTH_TOKEN='<AnyRouter token>' \
claude

# CLIProxyAPI
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/cpa \
claude
```

如已修改端口，请替换 `8765`。配置台中设置了路由密钥时，网关会替换发往上游的客户端凭据；路由密钥为空时则保持客户端凭据不变。

检查服务是否就绪：

```bash
curl http://127.0.0.1:8765/healthz
# ok
```

## 配置文件

默认路径：

```text
macOS：~/Library/Application Support/cc-automux/config.json
Linux：~/.config/cc-automux/config.json；设置了 $XDG_CONFIG_HOME 时为 $XDG_CONFIG_HOME/cc-automux/config.json
```

可以把 `CC_AUTOMUX_CONFIG` 设置为绝对路径以使用其它位置。建议通过配置台修改。

默认结构：

```json
{
  "listen_addr": "127.0.0.1:8765",
  "enabled": true,
  "log_max_bytes": 104857600,
  "anyrouter": {
    "entrances": [
      "https://anyrouter.top",
      "https://a-ocnfniawgw.cn-shanghai.fcapp.run"
    ],
    "accounts": []
  },
  "cpa": {
    "upstream": "https://127.0.0.1:8317",
    "key": "",
    "ca_path": ""
  },
  "classifier": {
    "target_base_url": "",
    "target_key": "",
    "model_override": "",
    "target_type": "",
    "target_ca_path": "",
    "target_insecure_skip_verify": false
  }
}
```

主要规则：

- `listen_addr` 必须使用 `127.0.0.1`，端口范围为 `1` 到 `65535`。
- AnyRouter 入口必须是互不重复的 `http` 或 `https` URL，且至少保留一个。
- 非空 AnyRouter 账号标签必须唯一；有密钥但无标签的账号会自动获得 `acct-N` 标签。
- `target_type` 接受空值/`auto`、`anyrouter`、`cpa` 或 `generic`。
- 分类器目标的 CA 文件与 `target_insecure_skip_verify` 不能同时启用。
- 未知 JSON 字段和尾随的额外 JSON 内容会被拒绝。

密钥保存在这个本地文件中，不要提交已经填入真实密钥的配置。

### 首次初始化

二进制除 `CC_AUTOMUX_CONFIG` 外不读取任何环境变量。配置文件不存在时由 `cc-automux init` 创建，安装器会代为执行。`--listen-addr` 与 `--log-max-bytes` 设置初始服务值，`--generate-management-key` 自动生成 management key 而不再提示输入：

```bash
./dist/cc-automux init --generate-management-key --listen-addr 127.0.0.1:8765
```

配置文件建立后即以文件内容为准，`init` 不会再改动它。

## 服务与日志

```bash
./scripts/status.sh
./scripts/stop.sh
./scripts/start.sh
```

macOS 安装路径：

```text
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/cc-automux.log
~/Library/Logs/cc-automux/cc-automux.log.1
~/Library/Logs/cc-automux/bootstrap.log
```

Linux 安装路径，设置了 `XDG_CONFIG_HOME` 与 `XDG_STATE_HOME` 时按其取值：

```text
~/.config/cc-automux/bin/cc-automux
~/.config/cc-automux/config.json
~/.config/systemd/user/cc-automux.service
~/.local/state/cc-automux/cc-automux.log
~/.local/state/cc-automux/cc-automux.log.1
```

进程自行管理两个 JSON Lines 文件。结构化日志历史通过需要认证的 `GET /api/v1/logs` 读取，实时记录通过需要认证的 `GET /api/v1/logs/stream` 推送，单条完整记录通过 `GET /api/v1/logs/record` 读取；不提供命令行日志查看脚本。超长字段在历史与推送中都会设上限，完整内容按引用单独获取。

`GET /api/v1/status` 会报告进程当前是否还能写入自己的日志。日志写入失败不会中断网关，因此该字段是让日志失效变得可见的唯一途径，否则它看起来只是最近没有新日志。

结构化日志可用之前的致命启动错误写入 stderr：macOS 上进入 `bootstrap.log`，安装器每次运行都会截断它；Linux 上进入 journal，用 `journalctl --user -u cc-automux.service` 查看。

## 升级与卸载

重新构建后再次运行安装器，即可替换已安装的二进制和服务注册，同时保留现有配置：

```bash
./scripts/install.sh
```

卸载服务、二进制、配置和日志：

```bash
./scripts/uninstall.sh
```

保留日志：

```bash
./scripts/uninstall.sh --keep-logs
```

macOS 上 Finder 可用时，卸载器会把文件移入废纸篓；在无图形界面的会话中，它会警告后永久删除相同的明确目标。Linux 上始终是永久删除。通过 `CC_AUTOMUX_CONFIG` 放在应用目录之外的配置文件不会被删除。

## 基础排障

- **服务无法启动：**运行 `./scripts/status.sh`，然后在 macOS 上检查 `~/Library/Logs/cc-automux/bootstrap.log`，在 Linux 上检查 `journalctl --user -u cc-automux.service`。
- **重新安装后新端口或上游没有生效：**现有配置优先，请在 `/admin` 中修改。
- **出现 `401` 或 `403`：**检查路由密钥/账号，以及使用的 `/any` 或 `/cpa` Base URL。
- **手动修改 JSON 后没有生效：**依次运行 `./scripts/stop.sh` 和 `./scripts/start.sh`。
- **上游暂时不可用：**使用 management 认证检查 `GET /api/v1/logs`；AnyRouter 会在传输错误、`429` 或 `5xx` 时自动切换入口。

配置台没有单独认证，并会向本地浏览器返回已配置密钥。它依赖仅回环监听提供边界；不要通过反向代理或隧道暴露该端口。
