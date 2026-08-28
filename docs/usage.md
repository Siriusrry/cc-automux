# Usage

[简体中文](usage.zh-CN.md) · [Project overview](../README.md)

## Requirements

- macOS for the LaunchAgent scripts.
- Go 1.22 or newer when building from source.
- An AnyRouter account, a running CLIProxyAPI instance, or another configured classifier target.

## Build and run

Build the binary:

```bash
go test ./...
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-auto-mode-shim ./cmd/cc-auto-mode-shim
```

Run it in the foreground:

```bash
./dist/cc-auto-mode-shim
```

Or install the built binary as a per-user macOS LaunchAgent:

```bash
./scripts/install.sh
```

The installer is non-interactive. Optional first-install values are:

```bash
PORT=9000 \
CLIPROXY_UPSTREAM=https://127.0.0.1:8317 \
LOG_MAX_MB=200 \
./scripts/install.sh
```

These values seed a new configuration. Reinstalling preserves an existing `config.json`.

## Configure the gateway

Open the local configuration desk:

```bash
open http://127.0.0.1:8765/admin
```

Configure only the routes you use:

- **AnyRouter:** ordered entrance URLs and zero or more account labels/keys. New sessions are distributed across usable accounts and remain sticky to one account.
- **CPA:** CLIProxyAPI upstream URL, one optional key, and an optional CA file.
- **Classifier interception:** normally keep each provider's default destination. A single target can instead define its own URL, key, platform type, model override, and TLS settings.
- **Service:** Active/Pass-through mode, loopback port, and per-file log limit.

Most saves apply immediately. Changing the port or log limit automatically restarts the process in place. Editing `config.json` directly is not watched; restart the service after a manual edit.

Pass-through mode keeps prefix routing and AnyRouter entrance failover, but disables gateway-managed credentials, account rotation, and compatibility rewrites.

## Connect Claude Code

Use one base URL per Claude Code process:

```bash
# AnyRouter
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/any \
ANTHROPIC_AUTH_TOKEN='<AnyRouter token>' \
claude

# CLIProxyAPI
ANTHROPIC_BASE_URL=http://127.0.0.1:8765/cpa \
claude
```

If you changed the port, replace `8765`. A key configured in the desk replaces the client's upstream credential; an empty route key leaves the client credential unchanged.

Check readiness:

```bash
curl http://127.0.0.1:8765/healthz
# ok
```

## Configuration file

Default path:

```text
~/Library/Application Support/cc-auto-mode-shim/config.json
```

Set `CC_AUTO_SHIM_CONFIG` to an absolute path to use another location. The configuration desk is the recommended editor.

Default shape:

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

Key rules:

- `listen_addr` must use `127.0.0.1` and a port from `1` to `65535`.
- AnyRouter entrances must be distinct `http` or `https` URLs; at least one is required.
- Non-empty AnyRouter account labels must be unique. A keyed account with no label receives an `acct-N` label.
- `target_type` accepts empty/`auto`, `anyrouter`, `cpa`, or `generic`.
- A classifier target CA file and `target_insecure_skip_verify` cannot be enabled together.
- Unknown JSON fields and trailing JSON content are rejected.

Keys are stored in this local file. Do not commit a populated configuration.

### First-run environment values

When the configuration file does not exist, the binary can seed it from:

| Variable | Default | Purpose |
|---|---|---|
| `CC_AUTO_SHIM_LISTEN` | `127.0.0.1:8765` | Initial listen address. |
| `CC_ANYROUTER_SHIM_UPSTREAM` | Two default AnyRouter entrances | Comma-separated initial entrance list. |
| `CC_CLIPROXY_SHIM_UPSTREAM` | `https://127.0.0.1:8317` | Initial CPA upstream. |
| `CC_CLIPROXY_SHIM_CA` | empty | Initial CPA CA file. |
| `CC_AUTO_SHIM_LOG_MAX_BYTES` | `104857600` | Initial per-file log cap in bytes. |

After the file exists, its routing, listen address, and log cap are authoritative. `CC_AUTO_SHIM_STDOUT_LOG` and `CC_AUTO_SHIM_STDERR_LOG` select file-log paths and are normally set by the LaunchAgent.

## Service and logs

```bash
./scripts/status.sh
./scripts/logs.sh
./scripts/logs.sh --err --last 200
./scripts/stop.sh
./scripts/start.sh
```

Installed paths:

```text
~/Library/Application Support/cc-auto-mode-shim/bin/cc-auto-mode-shim
~/Library/Application Support/cc-auto-mode-shim/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-auto-mode-shim.plist
~/Library/Logs/cc-auto-mode-shim/stdout.log
~/Library/Logs/cc-auto-mode-shim/stderr.log
```

The `/admin/logs` page shows recent lines from the current process. `scripts/logs.sh` reads the log files, including older retained lines.

## Upgrade and uninstall

After rebuilding, rerun the installer to replace the installed binary and LaunchAgent while preserving the existing configuration:

```bash
./scripts/install.sh
```

Uninstall the service, binary, configuration, and logs:

```bash
./scripts/uninstall.sh
```

Keep logs with:

```bash
./scripts/uninstall.sh --keep-logs
```

The uninstaller uses the macOS Trash when Finder is available. In a headless session it warns and permanently removes the same named targets. A configuration stored outside the application directory through `CC_AUTO_SHIM_CONFIG` is not removed.

## Basic troubleshooting

- **The service does not start:** run `./scripts/status.sh` and `./scripts/logs.sh --err --no-follow`.
- **A reinstall appears to ignore a new port or upstream:** the existing configuration is authoritative; change it in `/admin`.
- **`401` or `403`:** verify the route key/account and the selected `/any` or `/cpa` base URL.
- **A manual JSON edit has no effect:** restart with `./scripts/stop.sh` followed by `./scripts/start.sh`.
- **An upstream is temporarily unavailable:** inspect stderr. AnyRouter failover occurs automatically for transport errors, `429`, and `5xx` responses.

The configuration desk has no separate authentication and returns configured keys to the local browser. It is protected by the loopback-only listener; do not expose the port through a reverse proxy or tunnel.
