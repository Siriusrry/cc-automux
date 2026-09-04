# Usage

[简体中文](usage.zh-CN.md) · [Project overview](../README.md)

## Requirements

- macOS or Linux for the install scripts. They register a per-user LaunchAgent on macOS and a per-user systemd unit on Linux.
- Go 1.22 or newer when building from source.
- An AnyRouter account, a running CLIProxyAPI instance, or another configured classifier target.

## Build and run

Build the binary:

```bash
go test ./...
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
```

Run it in the foreground:

```bash
./dist/cc-automux
```

Or install the built binary as a per-user service, a LaunchAgent on macOS or a `systemd --user` unit on Linux:

```bash
./scripts/install.sh
```

On a first installation the installer asks whether to generate a management key; declining lets you type one at the binary's visible prompt. The configuration file is written by `cc-automux init`, never by the script itself. Reinstalling keeps an existing `config.json` and its keys unchanged. The only environment override is `CC_AUTOMUX_CONFIG`, an absolute path to the configuration file.

## Configure the gateway

Open the local configuration desk:

```bash
open http://127.0.0.1:8765/admin
```

Configure only the routes you use:

- **AnyRouter:** ordered entrance URLs and zero or more account labels/keys. New sessions are distributed across usable accounts and remain sticky to one account.
- **CPA:** CLIProxyAPI upstream URL, one optional key, and an optional CA file.
- **Classifier interception:** normally keep each provider's default destination. A single target can instead define its own URL, key, platform type, model override, and TLS settings.
- **Service:** Active/Pass-through mode, loopback port, and the log size limit, which is shared by the active log file and its one archived generation.

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
macOS: ~/Library/Application Support/cc-automux/config.json
Linux: ~/.config/cc-automux/config.json, or $XDG_CONFIG_HOME/cc-automux/config.json when that variable is set
```

Set `CC_AUTOMUX_CONFIG` to an absolute path to use another location. The configuration desk is the recommended editor.

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

### First-run initialization

The binary reads no environment variable other than `CC_AUTOMUX_CONFIG`. A missing configuration file is created by `cc-automux init`, which the installer runs for you. `--listen-addr` and `--log-max-bytes` set the initial service values, and `--generate-management-key` creates the management key instead of prompting for it:

```bash
./dist/cc-automux init --generate-management-key --listen-addr 127.0.0.1:8765
```

Once the file exists it is authoritative, and `init` leaves it untouched.

## Service and logs

```bash
./scripts/status.sh
./scripts/stop.sh
./scripts/start.sh
```

Installed paths on macOS:

```text
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/cc-automux.log
~/Library/Logs/cc-automux/cc-automux.log.1
~/Library/Logs/cc-automux/bootstrap.log
```

Installed paths on Linux, honouring `XDG_CONFIG_HOME` and `XDG_STATE_HOME` when set:

```text
~/.config/cc-automux/bin/cc-automux
~/.config/cc-automux/config.json
~/.config/systemd/user/cc-automux.service
~/.local/state/cc-automux/cc-automux.log
~/.local/state/cc-automux/cc-automux.log.1
```

The process owns the two JSON Lines files. Structured log history is read through authenticated `GET /api/v1/logs`, live records use authenticated `GET /api/v1/logs/stream`, and one complete record is read with `GET /api/v1/logs/record`; there is no command-line log-viewing script. Oversized fields are bounded in both the history and the stream, and the complete value is fetched by reference.

`GET /api/v1/status` reports whether the process can still write its own log. A log write failure does not stop the gateway, so that field is how a broken log becomes visible instead of looking like an idle period.

Fatal startup errors that happen before structured logging is ready go to stderr. On macOS they land in `bootstrap.log`, which the installer truncates on every run. On Linux they go to the journal, read with `journalctl --user -u cc-automux.service`.

## Upgrade and uninstall

After rebuilding, rerun the installer to replace the installed binary and service registration while preserving the existing configuration:

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

On macOS the uninstaller uses the Trash when Finder is available; in a headless session it warns and permanently removes the same named targets. On Linux removal is always permanent. A configuration stored outside the application directory through `CC_AUTOMUX_CONFIG` is not removed.

## Basic troubleshooting

- **The service does not start:** run `./scripts/status.sh`, then inspect `~/Library/Logs/cc-automux/bootstrap.log` on macOS or `journalctl --user -u cc-automux.service` on Linux.
- **A reinstall appears to ignore a new port or upstream:** the existing configuration is authoritative; change it in `/admin`.
- **`401` or `403`:** verify the route key/account and the selected `/any` or `/cpa` base URL.
- **A manual JSON edit has no effect:** restart with `./scripts/stop.sh` followed by `./scripts/start.sh`.
- **An upstream is temporarily unavailable:** inspect `GET /api/v1/logs` with management authentication. AnyRouter failover occurs automatically for transport errors, `429`, and `5xx` responses.

The configuration desk has no separate authentication and returns configured keys to the local browser. It is protected by the loopback-only listener; do not expose the port through a reverse proxy or tunnel.
