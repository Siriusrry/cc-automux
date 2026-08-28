# Script reference

[简体中文](README.zh-CN.md) · [Usage guide](../docs/usage.md)

These scripts manage the per-user macOS LaunchAgent for CC AutoMux. Run them from any directory; paths are resolved relative to the repository.

## Scripts

| Script | Purpose |
|---|---|
| `install.sh` | Install the existing binary, render the LaunchAgent, and start it. |
| `start.sh` | Start an installed LaunchAgent. |
| `stop.sh` | Stop/unload the LaunchAgent. |
| `status.sh` | Print the LaunchAgent status. |
| `logs.sh` | Read or follow stdout/stderr logs. |
| `uninstall.sh` | Stop the service and remove its installed files. |
| `_lib.sh` | Shared implementation helpers; do not run directly. |

The local smoke test is outside this directory: `../tests/smoke/run.sh`.

## Install

Build the binary first:

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/cc-automux ./cmd/cc-automux
```

Install non-interactively:

```bash
./scripts/install.sh
```

Optional bootstrap values:

```bash
PORT=9000 \
CLIPROXY_UPSTREAM=https://127.0.0.1:8317 \
LOG_MAX_MB=200 \
./scripts/install.sh
```

| Variable | Default | Meaning |
|---|---:|---|
| `PORT` | `8765` | Loopback listen port. The host is always `127.0.0.1`. |
| `CLIPROXY_UPSTREAM` | `https://127.0.0.1:8317` | `/cpa` upstream URL. |
| `LOG_MAX_MB` | `100` | Maximum size of each file log in MB. |
| `CC_AUTOMUX_CONFIG` | unset | Optional absolute path for `config.json`. |

The installer expects an executable `dist/cc-automux`; it does not build, format, or test source files. On first launch, the binary seeds a missing configuration file from the bootstrap values. An existing configuration remains authoritative when the installer is run again.

Installed paths:

```text
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/stdout.log
~/Library/Logs/cc-automux/stderr.log
```

After installation, open `http://127.0.0.1:<port>/admin` to configure accounts, keys, and routes. If an existing configuration uses another port, use that configured port.

## Start, stop, and status

```bash
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
```

The LaunchAgent label is `com.Siriusrry.cc-automux`. The service is kept alive by launchd, so use `stop.sh` instead of killing the process directly.

## Logs

```bash
./scripts/logs.sh
./scripts/logs.sh --out
./scripts/logs.sh --err --last 200
./scripts/logs.sh --both --no-follow
```

| Option | Meaning |
|---|---|
| `--out` | Show only `stdout.log`. |
| `--err` | Show only `stderr.log`. |
| `--both` | Show both logs (default). |
| `--last N` | Show the last `N` lines (default: `100`). |
| `--no-follow` | Print once instead of following new lines. |
| `-h`, `--help` | Show command help. |

Press `Ctrl-C` to stop a following command. Normal events are written to stdout; failures and diagnostics are written to stderr.

## Uninstall

```bash
./scripts/uninstall.sh
```

Use `--keep-logs` to retain the log directory:

```bash
./scripts/uninstall.sh --keep-logs
```

The script stops the service and targets only this application's plist, application directory, and log directory. Finder moves them to the Trash when available. In a headless session, it warns and permanently removes those same targets. A configuration file outside the application directory, selected with `CC_AUTOMUX_CONFIG`, is not removed.

## Smoke test

Run the isolated local end-to-end test from the repository root:

```bash
GOCACHE=/tmp/cc-automux-go-cache ./tests/smoke/run.sh
```

It builds temporary binaries, uses a temporary port and configuration, and does not contact the network or the installed service.
