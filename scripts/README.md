# Script reference

[简体中文](README.zh-CN.md) · [Usage guide](../docs/usage.md)

These scripts install and manage CC AutoMux as a per-user service on macOS and
Linux. They detect the platform at run time and dispatch to the platform's own
service manager: a LaunchAgent on macOS, a `systemd --user` unit on Linux.
Registration is always per-user and never requests elevation. The scripts do not
implement configuration or construct JSON.

## Scripts

| Script | Purpose |
|---|---|
| install.sh | Install the binary, initialize v1 configuration through cc-automux init, register the per-user service, and start it. |
| start.sh | Start the installed service. |
| stop.sh | Stop the service. |
| status.sh | Print the service status. |
| uninstall.sh | Stop the service, remove its registration, and delete its installed files. |
| _lib.sh | Shared implementation helpers; do not run directly. |

## Install

Build the binary first:

~~~
go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
~~~

Run the installer:

~~~
./scripts/install.sh
~~~

On the first installation, the script asks whether to generate a high-strength
management key. Choosing manual entry uses the binary's visible prompt.

The installer calls the shared initialization core and never writes JSON itself.
An existing configuration and its keys are preserved on reinstall.

The only environment override is CC_AUTOMUX_CONFIG, which must be an absolute
path. The service configuration, including listen_addr and log_max_bytes, is
stored in config.json and changed through the Web console at `/management` or the management API.

Installed paths on macOS:

~~~
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/cc-automux.log
~/Library/Logs/cc-automux/cc-automux.log.1
~/Library/Logs/cc-automux/bootstrap.log
~~~

Installed paths on Linux, honouring XDG_CONFIG_HOME and XDG_STATE_HOME when set:

~~~
~/.config/cc-automux/bin/cc-automux
~/.config/cc-automux/config.json
~/.config/systemd/user/cc-automux.service
~/.local/state/cc-automux/cc-automux.log
~/.local/state/cc-automux/cc-automux.log.1
~~~

The registration carries only the optional CC_AUTOMUX_CONFIG override. It does
not seed legacy route or upstream environment variables.

The process writes structured JSON Lines to `cc-automux.log` and keeps one older
generation in `cc-automux.log.1`. The configured `log_max_bytes` is the combined
budget.

Fatal startup errors that happen before the structured log exists go to stderr,
which each platform collects differently:

- **macOS:** into `~/Library/Logs/cc-automux/bootstrap.log`. The installer
  truncates this file on every run. It carries only errors that need a human, so a
  previous round's output has no value for the current one.
- **Linux:** into the journal, read with `journalctl --user -u cc-automux.service`.
  journald applies its own system-wide capacity limit and rotation, so the unit
  names no log path.

## Start, stop, and status

~~~
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
~~~

The service is named com.Siriusrry.cc-automux on macOS and cc-automux.service on
Linux. It remains loopback-only on both.

Structured log history is read through authenticated `GET /api/v1/logs`; live
records use authenticated `GET /api/v1/logs/stream`, and one complete record is
read with `GET /api/v1/logs/record`. There is no command-line log-viewing script.
`GET /api/v1/status` reports whether the process can still write its own log.

## Uninstall

~~~
./scripts/uninstall.sh
./scripts/uninstall.sh --keep-logs
~~~

The uninstaller targets only this application's own registration file,
application directory, and log directory; shared parent directories are left
intact. A configuration outside the application directory selected with
CC_AUTOMUX_CONFIG is not removed.

On macOS the removed items go to the Trash and stay recoverable through Finder's
"Put Back". On Linux they are deleted permanently, since there is no equivalent
user-level Trash guarantee for arbitrary paths.
