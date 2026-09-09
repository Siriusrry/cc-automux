# 使用说明

[English](usage.md) · [README](../README.zh-CN.md)

## 安装或直接运行

使用 Go 1.22 或更新版本构建：

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
```

在 macOS 或 Linux 安装为用户级服务：

```bash
./scripts/install.sh
```

首次安装时输入或生成 management key；重新安装保留已有配置。macOS 使用 LaunchAgent，Linux 使用 `systemd --user`，安装即启用自启动。平台路径和卸载行为见[脚本说明](../scripts/README.zh-CN.md)。

也可以直接运行：

```bash
./dist/cc-automux init --generate-management-key
./dist/cc-automux
```

避免与已安装服务占用相同端口。需要独立配置时使用绝对路径：

```bash
./dist/cc-automux init --generate-management-key --config /absolute/path/config.json --listen-addr 127.0.0.1:8766
CC_AUTOMUX_CONFIG=/absolute/path/config.json ./dist/cc-automux
```

`init` 不带 `--generate-management-key` 时交互输入密钥；重复初始化不会覆盖已有配置和密钥。`./dist/cc-automux --version` 显示嵌入的产品版本。

## 登录并连接 Claude Code

打开 [http://127.0.0.1:8765/management](http://127.0.0.1:8765/management)，按实际配置调整端口，使用 management key 登录。默认只保存到标签页会话；勾选 **Remember on this device** 才持久保存在该浏览器。**Sign out** 清除保存的密钥。

1. 在 **Service** 设置或生成 gateway key，它必须与 management key 不同。未配置时 Messages 请求返回 `503 gateway_not_configured`。
2. 在 **Providers** 添加兼容上游、上游密钥和准确的模型名。
3. 在 **Claude Code** 创建 Profile，填写 Haiku、Sonnet、Opus、Fable 四个必填模型映射，指向 Provider 声明的模型；Subagent、Teammate 映射可选。
4. 激活 Profile，将网关地址、密钥和模型映射写入选定的 Claude Code 配置文件。
5. 启动新的 Claude Code 会话，使其读取配置。

手动配置时，Base URL 只填网关基础地址，不附加 endpoint：

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

如果不激活 Profile，需要另行设置 Claude Code 的模型映射。

## 控制台页面

总览地址为 `/management`；其他页面使用 `/management#/providers`、`/management#/logs` 等 hash 路径，`#` 前没有斜杠。

| 页面 | 用途 |
|---|---|
| Overview | 运行状态、Provider 数量、路由图和配置告警。 |
| Providers | 增删改上游，设置模型、优先级和补丁，查看健康与会话诊断。 |
| Auto Mode | 为分类器选择 Off、Provider pool 或 Fixed provider。 |
| Claude Code | 选择配置路径、遥测设置，创建、编辑并激活模型映射 Profile。 |
| Logs | 筛选历史、跟随实时日志、展开错误和加载更早记录。 |
| Service | 设置监听端口、日志容量和密钥，查看运行信息与配置 JSON。 |

带保存条的表单通过 **Save changes** 提交，**Revert** 放弃草稿。行内开关和 Profile 操作按界面提示立即生效；配置路径通过 **Apply path** 提交。保存失败时保留输入并显示错误。其他窗口修改了同一配置字段时，先重新加载当前值再提交；无关字段的并发变化会保留。

### Provider 与路由

普通上游必须支持 Anthropic Messages API。填写基础 URL，CC AutoMux 会追加 `/v1/messages`，不要重复填写该 endpoint。模型按大小写敏感的完整名称匹配。

优先级数值越大越优先；新会话在最高可用同级 Provider 中轮询。有合法 `X-Claude-Code-Session-Id` 的会话，对相同模型和请求类型保持粘性。高层不可用时才使用低层。普通请求在允许重试的错误下最多尝试三个不同 Provider。关闭健康冷却只取消失败导致的调度抑制，诊断仍保留。

TLS 默认使用系统根证书；自定义 CA 与跳过证书验证互斥。兼容补丁必须显式选择并按配置顺序执行，不会因 Provider 名称或 URL 自动启用。

### Auto mode

- **Off**：分类器请求返回 `503 auto_mode_not_configured`，普通请求仍走 Provider 池。
- **Provider pool**：设置共用分类器模型。启用且声明该模型的 Provider 参与候选，使用优先级、粘性及独立分类器健康通道；每个分类器请求最多尝试一个 Provider。
- **Fixed provider**：配置基础 URL、密钥、协议、TLS 和可选分类器补丁。该目标只服务分类器，不进入普通 Provider 池。

Anthropic Messages 固定目标直接可用。OpenAI Responses 与 OpenAI-compatible 协议转换尚未实现，允许保存配置，但分类器请求会返回 `501 protocol_not_implemented`。解析或转换失败时不会伪造 allow/block 结果。

### Claude Code 文件与 Profile

默认目标是当前用户的 `.claude/settings.json`，也可选择自定义绝对路径。激活只更新受管的网关、模型和遥测字段，保留其他值。首次修改已有文件前建立同目录 `.cc-automux.bak`，已有备份不覆盖；首次创建配置文件不生成备份。

保存 Profile 不等于激活。**Active** 表示已核对受管字段；外部修改导致不一致时，下次检查会清空激活状态。打开或返回 Claude Code 页面会重新检查。修改网关地址、密钥或当前 Profile 后可能需要重新激活。

**Disable Claude Code telemetry** 控制开关旁展示的四个字段，激活 Profile 时写入。

### 日志与重启

进程以 JSON Lines 写入一个活动日志文件和一个归档文件。通过 **Logs** 查看历史与实时记录，筛选同时作用于两者。向上滚动时暂停跟随并暂存新增记录；返回底部恢复。出现丢弃或缓冲区已满提示时重新加载视图。

长字段默认有界显示。**Show complete** 获取完整记录；记录已被轮转移除时界面会明确说明。日志健康告警表示写入失败，即使网关本身仍在服务。

修改监听端口或日志容量会重启进程并中断在途请求。控制台等待新配置生效；改端口后自动打开新地址，需要重新登录。只轮换 management key 时，当前控制台切换到新密钥，其他会话需重新登录。

## 配置与管理 API

默认配置路径：

| 平台 | 路径 |
|---|---|
| macOS | `~/Library/Application Support/cc-automux/config.json` |
| Linux | `$XDG_CONFIG_HOME/cc-automux/config.json`，未设置时为 `~/.config/cc-automux/config.json` |

`CC_AUTOMUX_CONFIG` 可通过绝对路径覆盖配置位置。运行时通过控制台或管理 API 应用变更；直接编辑磁盘配置需要重启进程。

最小配置结构如下，使用前替换示例 management key：

```json
{
  "schema_version": 1,
  "service": {"listen_addr": "127.0.0.1:8765", "log_max_bytes": 104857600},
  "auth": {"gateway_key": "", "management_key": "replace-with-a-random-management-key"},
  "auto_mode": {"mode": "disabled", "model": ""},
  "harnesses": {"claude_code": {"path_mode": "default", "settings_path": "", "disable_telemetry": true, "profiles": []}},
  "providers": []
}
```

服务必须配置 management key，只能绑定 `127.0.0.1`。Unix 配置文件权限为 `0600`。配置文件和密钥不要进入源码版本控制。

管理请求使用 `Authorization: Bearer <management key>`：

- `GET /api/v1/status`：运行、重启和日志健康状态。
- `GET/PUT /api/v1/config`：完整配置读取与替换。
- `/api/v1/providers`、`/api/v1/providers/{id}`：Provider CRUD。
- `/api/v1/harnesses/claude-code` 及其 `/profiles` 资源：设置与 Profile。
- `GET /api/v1/logs`、`/api/v1/logs/stream`、`/api/v1/logs/record?ref=…`：历史、SSE 和完整记录。

Config GET 包含服务端只读 `active_profile_id`，构造 PUT 时须移除，并保留不打算修改的字段。把 GET 返回的 ETag 作为 `If-Match` 发送，可让过期替换返回 `412 configuration_changed`。成功 PUT 返回 `200` 表示已应用，`202` 表示等待重启生效。

## 排障

- **控制台打不开**：检查 `./scripts/status.sh`、实际端口和启动错误。macOS 早期错误位于 `~/Library/Logs/cc-automux/bootstrap.log`；Linux 使用 `journalctl --user -u cc-automux.service`。
- **登录失败或 401**：控制台使用 management key，Claude Code 使用 gateway key。Management key 轮换会使其他会话失效。
- **模型不可用**：检查准确模型名、Provider 是否启用及健康状态，在 Providers 查看上游原始错误。
- **Auto mode 失败**：配置分类器模型及可用候选或 Anthropic 固定目标；未实现的 OpenAI 协议返回 501。
- **保存冲突或 412**：必要时先保留草稿，再重新加载当前配置并应用需要的改动。
- **Profile 不同步**：检查目标路径并重新激活；写入失败会报错，不会假报 Active。
- **重新安装没有重置设置**：安装器会保留已有配置和密钥。

服务启停、升级、日志目录及卸载行为见[脚本说明](../scripts/README.zh-CN.md)。
