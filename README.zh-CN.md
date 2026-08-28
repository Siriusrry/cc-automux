# cc-auto-mode-shim

[English](README.md)

`cc-auto-mode-shim` 是一个仅监听本机回环地址的轻量网关，用于通过 AnyRouter 或 CLIProxyAPI（CPA）使用 Claude Code auto mode。它修复分类器请求与响应的兼容性，同时保持普通模型流量的流式转发。

## 功能

- `/any`：AnyRouter 路由，支持有序粘性故障切换和按会话粘性的多账号密钥轮询。
- `/cpa`：CLIProxyAPI 路由，支持一个可选的网关托管密钥。
- 支持 Claude 与 GPT 模型族的 auto-mode 分类器兼容处理。
- 可选的统一分类器目标，支持独立 URL、密钥、平台类型、模型覆盖和 TLS 设置。
- 本地 Web 配置台，可查看运行状态和实时日志。
- 全局 Active/Pass-through 开关。
- JSON 配置持久化并实时应用；端口和日志大小变更会原地重启进程。

服务默认监听 `127.0.0.1:8765`，且不接受非回环监听地址。

## 快速开始

需要 Go 1.22 或更高版本、至少一个可用的 AnyRouter 或 CLIProxyAPI 上游；使用随附的 LaunchAgent 脚本时需要 macOS。

```bash
go test ./...
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-auto-mode-shim ./cmd/cc-auto-mode-shim
./scripts/install.sh
```

打开配置台，填写所用上游的 URL 和凭据：

```bash
open http://127.0.0.1:8765/admin
```

然后通过其中一个路由启动 Claude Code：

```bash
# AnyRouter
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/any \
ANTHROPIC_AUTH_TOKEN='<AnyRouter token>' \
claude

# CLIProxyAPI
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/cpa \
claude
```

检查本地服务是否就绪：

```bash
curl http://127.0.0.1:8765/healthz
# ok
```

配置台中设置了路由密钥时，网关会替换发往该上游的客户端凭据；未设置时则原样转发客户端凭据。

## 服务命令

```bash
./scripts/status.sh
./scripts/logs.sh
./scripts/stop.sh
./scripts/start.sh
./scripts/uninstall.sh
```

## 文档

- [使用说明](docs/usage.zh-CN.md)
- [脚本说明](scripts/README.zh-CN.md)

本项目只处理明确指向 `/any` 或 `/cpa` 的 Claude Code 流量；其它客户端应继续使用各自的上游配置。
