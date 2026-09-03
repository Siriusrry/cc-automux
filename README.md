# cc-auto-mode-shim

[简体中文](README.zh-CN.md)

`cc-auto-mode-shim` is a loopback-only local gateway for using Claude Code auto mode with AnyRouter and CLIProxyAPI (CPA). It fixes classifier request/response compatibility while keeping normal model traffic streaming.

## Features

- `/any`: AnyRouter routing with ordered sticky failover and session-sticky multi-account keys.
- `/cpa`: CLIProxyAPI routing with one optional shim-managed key.
- Claude- and GPT-family auto-mode classifier compatibility.
- Optional single classifier target with its own URL, key, platform type, model override, and TLS settings.
- Local web desk for configuration, runtime status, and live logs.
- Global Active/Pass-through switch.
- Persistent JSON configuration with live apply; port and log-size changes restart the process in place.

The service listens on `127.0.0.1:8765` by default and never accepts a non-loopback listen address.

## Quick start

Requirements: Go 1.22 or newer, macOS when using the bundled LaunchAgent scripts, and at least one usable AnyRouter or CLIProxyAPI upstream.

```bash
go test ./...
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-auto-mode-shim ./cmd/cc-auto-mode-shim
./scripts/install.sh
```

Open the configuration desk and add the upstream URL and credentials you use:

```bash
open http://127.0.0.1:8765/admin
```

Then start Claude Code through one route:

```bash
# AnyRouter
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/any \
ANTHROPIC_AUTH_TOKEN='<AnyRouter token>' \
claude

# CLIProxyAPI
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/cpa \
claude
```

Check that the local service is ready:

```bash
curl http://127.0.0.1:8765/healthz
# ok
```

When a route key is configured in the desk, the shim replaces the client credential for that upstream. With no configured key, the client credential is forwarded unchanged.

## Service commands

```bash
./scripts/status.sh
./scripts/stop.sh
./scripts/start.sh
./scripts/uninstall.sh
```

## Documentation

- [Usage guide](docs/usage.md)
- [Script reference](scripts/README.md)

This gateway handles only Claude Code traffic explicitly pointed at `/any` or `/cpa`. Other clients should continue using their own upstream configuration.
