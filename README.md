# CC AutoMux

[简体中文](README.zh-CN.md)

A multi-provider gateway for Claude Code, with model routing, auto-mode extensions, and compatibility fixes.

CC AutoMux runs on your computer, forwards Anthropic Messages requests, and provides a local web console for providers, model mappings, auto-mode compatibility settings, and logs. The console is embedded in the Go binary; it needs no separate frontend server or Node.js installation.

## Features

- **Multiple providers and model routing:** add several providers and route each model to its upstream, using different services together. Session stickiness, failover, and round-robin load balancing within the same priority keep requests distributed across available providers.
- **Auto-mode extensions and compatibility:** restore auto mode when compatibility issues with unofficial APIs prevent it from working. Choose the classifier model independently and route it through the provider pool or a fixed upstream. Fixed classifier targets can use Anthropic Messages, OpenAI Responses, or OpenAI-compatible APIs, allowing custom OpenAI and other models.
- **One-click Claude Code profiles:** save multiple model-mapping profiles and quickly switch gateway connections, model mappings, and telemetry settings for different tasks.
- **Local management console:** manage providers, model mappings, and auto-mode compatibility settings; inspect model routing, provider health, and sessions; search request history, follow live errors, and configure service ports and access keys.

## Quick start

Supports macOS 12 or newer and Linux with `systemd --user`, with amd64 and arm64 binaries. Installation requires Bash, curl, tar, a SHA-256 utility, and a terminal in a logged-in user session.

Install or upgrade:

```bash
curl -fsSL https://raw.githubusercontent.com/Siriusrry/cc-automux/main/scripts/install.sh | bash
```

On first installation, choose a port (default `8765`) and generate or enter a **management key**. The installer enables login autostart, checks service readiness, and prints the actual Web UI address. Keep the key for signing in. Upgrades preserve the configuration without asking for the port or keys again.

Open the address printed by the installer, then:

1. Sign in with the **management key**.
2. In **Service**, set or generate a separate **gateway key**.
3. In **Providers**, add upstreams and their exact model names.
4. Configure **Auto Mode** if needed.
5. In **Claude Code**, create a model-mapping profile and activate it.
6. Start a new Claude Code session.

Alternatively, point Claude Code at the gateway manually, using your configured port:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8765 \
ANTHROPIC_AUTH_TOKEN='<gateway key>' \
claude
```

Claude Code's model names must match those declared by your providers. The service only listens on `127.0.0.1`; the gateway key authenticates Claude Code requests and the management key authenticates the console.

## Local builds

Developers can build with Go 1.22 or newer and explicitly install the local `dist/` binary:

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh --local
```

`--local` neither downloads nor builds, and installs the selected binary even when its version string is unchanged. A later public upgrade requires a higher version that supports the existing configuration. Switching to the same or an older public version requires manual preparation.

## Service management

The installer prints the full paths to the installed start, stop, status, and uninstall scripts. They live in the application's `scripts/` directory and do not depend on the source checkout or a temporary download. See the [script reference](scripts/README.md) for paths, commands, and removal behavior.

## Documentation

- [Usage guide](docs/usage.md): setup, console pages, configuration, and troubleshooting.
- [Script reference](scripts/README.md): installation, upgrades, platform paths, and service operations.
