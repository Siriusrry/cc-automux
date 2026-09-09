# CC AutoMux

[简体中文](README.zh-CN.md)

A multi-provider gateway and auto-mode compatibility layer for Claude Code.

CC AutoMux runs on your computer, forwards Anthropic Messages requests, and provides a local web console for providers, model mappings, auto mode, and logs. The console is embedded in the Go binary; it needs no separate frontend server or Node.js installation.

## Features

- Providers with individual URLs, keys, model lists, priorities, TLS settings, and optional compatibility patches.
- Model-based routing, same-priority round-robin, session stickiness, health cooldowns, and request failover.
- Auto-mode classifier routing through the provider pool or a fixed target.
- Claude Code profiles that write the gateway address, key, model mappings, and telemetry settings into `settings.json`, with a one-time backup of an existing file.
- A light/dark web console with provider diagnostics, configuration editing, and searchable live/history logs.
- Atomic configuration updates and automatic process restart when the listener or log size limit changes.

Ordinary providers must accept the **Anthropic Messages API** directly. Fixed classifier targets can use Anthropic Messages; OpenAI Responses and OpenAI-compatible conversion are not implemented yet. Selecting either unsupported protocol returns `501 protocol_not_implemented` for classifier requests.

## Quick start

Requirements: Go 1.22 or newer to build, macOS or Linux to run the binary with the supplied service scripts, and an Anthropic Messages-compatible upstream.

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh
```

The installer initializes the configuration, asks you to enter or generate a management key, and starts a per-user service. Keep that key for signing into the console.

Open [http://127.0.0.1:8765/management](http://127.0.0.1:8765/management) using the configured port if it differs from the default. Then:

1. Sign in with the **management key**.
2. In **Service**, set or generate a separate **gateway key**.
3. In **Providers**, add an upstream and its exact model names.
4. In **Claude Code**, create a model-mapping profile and activate it.
5. Configure **Auto Mode** if you use Claude Code auto mode.

Alternatively, point Claude Code at the gateway manually:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

Configure Claude Code's model names to match the models declared by your providers. The gateway receives requests at `POST /v1/messages`.

The service only listens on `127.0.0.1`. The gateway key and management key protect different interfaces and cannot replace each other.

## Service commands

```bash
./scripts/status.sh
./scripts/stop.sh
./scripts/start.sh
./scripts/uninstall.sh
```

## Documentation

- [Usage guide](docs/usage.md): setup, console pages, configuration, and troubleshooting.
- [Script reference](scripts/README.md): installation, platform paths, and service operations.
