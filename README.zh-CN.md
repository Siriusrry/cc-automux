# CC AutoMux

[English](README.md)

面向 Claude Code 的多 Provider 网关与 auto mode 兼容层。

CC AutoMux 在本机转发 Anthropic Messages 请求，通过本地 Web 控制台管理 Provider、模型映射、auto mode 和日志。前端嵌入 Go 二进制，无需独立前端服务或 Node.js。

## 功能

- 每个 Provider 独立配置 URL、密钥、模型列表、优先级、TLS 和可选兼容补丁。
- 按模型路由、同级轮询、会话粘性、健康冷却和请求故障切换。
- Auto mode 分类器使用 Provider 池或池外固定目标。
- Claude Code Profile 将网关地址、密钥、模型映射和遥测设置写入 `settings.json`；首次修改已有文件前创建一次性备份。
- 浅色/深色 Web 控制台，提供 Provider 诊断、配置编辑、历史日志筛选与实时日志。
- 配置原子更新；监听地址或日志容量改变时自动重启进程。

普通 Provider 必须直接支持 **Anthropic Messages API**。固定分类器目标可使用 Anthropic Messages；OpenAI Responses 和 OpenAI-compatible 协议转换尚未实现。选择后两种协议后，分类器请求会返回 `501 protocol_not_implemented`。

## 快速开始

要求：构建需要 Go 1.22 或更新版本；二进制及配套服务脚本面向 macOS、Linux；需要一个兼容 Anthropic Messages 的上游。

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh
```

安装器初始化配置，提示输入或生成 management key，并启动用户级服务。保存该密钥，用于登录控制台。

打开 [http://127.0.0.1:8765/management](http://127.0.0.1:8765/management)；如果修改过默认端口，请使用配置中的端口。然后：

1. 用 **management key** 登录。
2. 在 **Service** 设置或生成独立的 **gateway key**。
3. 在 **Providers** 添加上游及准确的模型名。
4. 在 **Claude Code** 创建模型映射 Profile 并激活。
5. 使用 Claude Code auto mode 时，配置 **Auto Mode**。

也可以手动让 Claude Code 连接网关：

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

Claude Code 的模型名须与 Provider 声明的模型匹配。网关接收 `POST /v1/messages` 请求。

服务只监听 `127.0.0.1`。Gateway key 与 management key 分别保护不同接口，不能互相替代。

## 服务命令

```bash
./scripts/status.sh
./scripts/stop.sh
./scripts/start.sh
./scripts/uninstall.sh
```

## 文档

- [使用说明](docs/usage.zh-CN.md)：配置、控制台页面、运行与排障。
- [脚本说明](scripts/README.zh-CN.md)：安装、平台路径与服务操作。
