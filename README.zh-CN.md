# CC AutoMux

[English](README.md)

面向 Claude Code 的多 Provider 网关，支持模型调度、auto mode 扩展与兼容修复。

CC AutoMux 在本机转发 Anthropic Messages 请求，通过本地 Web 控制台管理 Provider、模型映射、auto mode 修复设置和日志。前端嵌入 Go 二进制，无需独立前端服务或 Node.js。

## 功能

- **多 Provider 与模型调度**：添加多个 Provider，将不同模型的请求调度到对应上游，同时使用多家服务。支持会话粘性、故障转移和同优先级轮询式负载均衡。
- **Auto mode 扩展与兼容修复**：修复使用非官方 API 时 auto mode 不可用的问题；可独立选择分类器模型，使用 Provider 池或指定固定上游；固定分类器目标可使用 Anthropic Messages、OpenAI Responses 和 OpenAI-compatible，支持自定义 OpenAI 等模型。
- **一键切换 Claude Code 配置**：保存多套模型映射 Profile，一键切换网关连接、模型映射和遥测设置，满足不同任务下快速切换配置的需要。
- **本地管理控制台**：集中管理 Provider、模型映射和 auto mode 修复设置；查看模型路由、Provider 健康与会话诊断，检索历史日志、跟踪实时请求和错误，调整服务端口与访问密钥。

## 快速开始

支持 macOS 12 或更新版本，以及提供 `systemd --user` 的 Linux；提供 amd64、arm64 二进制。需要 Bash、curl、tar 和 SHA-256 校验工具，安装时使用已登录的用户终端。

安装与升级命令：

```bash
curl -fsSL https://raw.githubusercontent.com/Siriusrry/cc-automux/main/scripts/install.sh | bash
```

首次安装时选择监听端口（默认 `8765`），再自动生成或手动输入 **management key**。安装器启用用户登录自启，检查服务就绪后显示实际 Web UI 地址。保存该密钥，用于登录控制台。升级保留已有配置，无需重新设置端口或密钥。

打开安装器显示的地址，然后：

1. 用 **management key** 登录。
2. 在 **Service** 设置或生成独立的 **gateway key**。
3. 在 **Providers** 添加上游及准确的模型名。
4. 按需配置 **Auto Mode**。
5. 在 **Claude Code** 创建模型映射 Profile 并激活。
6. 启动新的 Claude Code 会话。

也可以手动让 Claude Code 连接网关，使用实际配置的端口：

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

Claude Code 的模型名须与 Provider 声明的模型匹配。服务只监听 `127.0.0.1`；gateway key 用于 Claude Code 请求，management key 用于管理控制台。

## 本地构建

开发者可使用 Go 1.22 或更新版本构建，并显式安装本地 `dist/` 产物：

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh --local
```

`--local` 不联网、不自动构建，同版本也会安装本次产物。以后通过公开安装命令升级时，目标版本须更高且支持现有配置；同版本切回公开版或降级需手动处理。

## 服务管理

安装器会显示已安装的启停、状态与卸载脚本的完整路径。这些脚本位于应用目录的 `scripts/` 下，不依赖源码或临时下载目录。具体路径、命令与卸载范围见[脚本说明](scripts/README.zh-CN.md)。

## 文档

- [使用说明](docs/usage.zh-CN.md)：配置、控制台页面、运行与排障。
- [脚本说明](scripts/README.zh-CN.md)：安装、升级、平台路径与服务操作。
