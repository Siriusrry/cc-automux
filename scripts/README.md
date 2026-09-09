# Script reference

[简体中文](README.zh-CN.md) · [Usage guide](../docs/usage.md)

CC AutoMux runs as a per-user LaunchAgent on macOS or a `systemd --user` service on Linux. Installation enables login autostart without requesting system elevation. A logged-in graphical session is required on macOS; Linux requires an available systemd user manager. Binaries are available for amd64 and arm64, with macOS 12 as the minimum macOS version.

## Install and upgrade

```bash
curl -fsSL https://raw.githubusercontent.com/Siriusrry/cc-automux/main/scripts/install.sh | bash
```

This command installs or upgrades to the latest complete public stable Release. It requires Bash, curl, tar, and either `sha256sum` or `shasum`; it does not require a source checkout, Go, or Node.js. Downloads use one fixed release version, and the installer checks SHA-256, platform, and binary version before proceeding.

On first installation, choose a port (default `8765`) and either generate a management key or enter one visibly. Prompts read from the controlling terminal, so `curl … | bash` works. First installation without a controlling terminal exits with an explanation. Configuration is initialized by the binary, not assembled in shell.

Existing configuration is checked with the target binary and kept. Upgrades retain port, keys, providers, profiles, auto-mode settings, and Claude Code paths; they do not activate profiles or edit Claude Code settings. An invalid configuration, unsupported version, or unfinished configuration restart stops installation before replacement. After downloading and checking the candidate, the installer preserves the old program and registration. Replacement or readiness failure restores them and reports failure. If recovery itself fails, it prints the retained recovery-directory path.

Success is reported only after authenticated service status matches the expected product, version, and listener, and the web console responds. The output includes the actual URL, configuration path, version, and installed maintenance commands. An already-current complete public installation is left running; an unhealthy service is reported separately rather than restarted without a change.

Concurrent installation and maintenance operations are rejected. If an interrupted operation leaves `~/.cc-automux-install.lock`, first check that no installer or maintenance script is still running and resolve any reported recovery state, then remove that empty lock directory and retry.

## Local development builds

```bash
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
./scripts/install.sh --local
```

Build with Go 1.22 or newer. `--local` reads only `dist/cc-automux` beside the script's project, never downloads or builds, and uses the same installed service and configuration. It installs the selected binary even if the version string is unchanged. Back up any program and configuration you need to retain before trying a local build.

Without `--local`, the installer always uses the public Release, even when the current directory contains `dist/`. Public upgrades from a local build require both a higher semantic version and configuration support: `v1.1.0-dev → v1.1.0` is forward; `v1.1.0-dev → v1.0.1` is a downgrade. Same-version local replacement and public downgrades are refused. Prepare the program and configuration manually when returning to an equal or older public version. A local installation with a missing binary cannot bypass the version check through repair.

## Installed locations

| Item | macOS | Linux default |
|---|---|---|
| Application | `~/Library/Application Support/cc-automux/` | `~/.config/cc-automux/` |
| Binary | Application + `bin/cc-automux` | Application + `bin/cc-automux` |
| Configuration | Application + `config.json` | Application + `config.json` |
| Maintenance scripts | Application + `scripts/` | Application + `scripts/` |
| Registration | `~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist` | `~/.config/systemd/user/cc-automux.service` |
| Logs | `~/Library/Logs/cc-automux/` | `~/.local/state/cc-automux/` |

On Linux, `XDG_CONFIG_HOME` selects the application and registration root; `XDG_STATE_HOME` selects the log root. `CC_AUTOMUX_CONFIG` selects an absolute configuration path on either platform. During an upgrade the existing registration and installed path record take precedence over a changed terminal environment. The registration pins configuration and Linux log paths for subsequent starts.

The application directory also holds user documentation, service templates, an `install-paths` record, and an `install-source` value of `public` or `local`. These are installation metadata, separate from product configuration. The installed scripts do not depend on the extracted download directory.

Structured logs are `cc-automux.log` and `cc-automux.log.1`. Their combined capacity is configured in the console. Startup errors before logging is available go to `bootstrap.log` on macOS (truncated when replacing an installation) or the systemd user journal on Linux:

```bash
journalctl --user -u cc-automux.service
```

## Start, stop, and status

Use the exact application directory printed by the installer. For the default macOS installation:

```bash
APP_DIR="$HOME/Library/Application Support/cc-automux"
"$APP_DIR/scripts/status.sh"
"$APP_DIR/scripts/stop.sh"
"$APP_DIR/scripts/start.sh"
```

For the default Linux installation, set `APP_DIR="$HOME/.config/cc-automux"` before running the same commands. `start.sh` checks readiness; `status.sh` reports supervisor state and authenticated gateway readiness. Request history and live logs are available in the console's **Logs** page.

## Uninstall

First change Claude Code's connection settings if it should no longer use this gateway. Then use the installed script:

```bash
"$APP_DIR/scripts/uninstall.sh"
# Or retain the application log directory:
"$APP_DIR/scripts/uninstall.sh" --keep-logs
```

Uninstall stops the service, removes login autostart, and removes the application directory, including its default configuration. Logs are removed unless `--keep-logs` is supplied. A custom configuration outside the application directory is kept. Claude Code's `settings.json` and its backup are not automatically restored or deleted.

On macOS removal uses the Trash through Finder when available, with permanent removal as the headless fallback. Linux removal is permanent. Shared parent directories are left intact.

## Script roles

| Script | Purpose |
|---|---|
| `install.sh` | Public download entry point, or explicit `--local` installation. |
| `start.sh`, `stop.sh`, `status.sh` | Operate the installed user service and check its state. |
| `uninstall.sh` | Remove the installation, optionally retaining logs. |
| `_install.sh`, `_lib.sh` | Shared implementation; use the entry points above. |
| `release.py` | Build and inspect platform archives and checksums; requires Python 3.11+ and Go. |
