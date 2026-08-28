# Script reference

[简体中文](README.zh-CN.md) · [Usage guide](../docs/usage.md)

These scripts manage the per-user macOS LaunchAgent for the current CC AutoMux
v1 service. They do not implement configuration or construct JSON.

## Scripts

| Script | Purpose |
|---|---|
| install.sh | Install the binary, initialize v1 configuration through cc-automux init, render the LaunchAgent, and start it. |
| start.sh | Start an installed LaunchAgent. |
| stop.sh | Stop and unload the LaunchAgent. |
| status.sh | Print the LaunchAgent status. |
| logs.sh | Read or follow stdout/stderr logs. |
| uninstall.sh | Stop the service and remove its installed files. |
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
stored in config.json and changed through the management API.

Installed paths:

~~~
~/Library/Application Support/cc-automux/bin/cc-automux
~/Library/Application Support/cc-automux/config.json
~/Library/LaunchAgents/com.Siriusrry.cc-automux.plist
~/Library/Logs/cc-automux/stdout.log
~/Library/Logs/cc-automux/stderr.log
~~~

The LaunchAgent carries only the optional CC_AUTOMUX_CONFIG override. It does
not seed legacy route or upstream environment variables.

## Start, stop, and status

~~~
./scripts/start.sh
./scripts/stop.sh
./scripts/status.sh
~~~

The LaunchAgent label is com.Siriusrry.cc-automux. The service remains
loopback-only.

## Logs

~~~
./scripts/logs.sh
./scripts/logs.sh --out
./scripts/logs.sh --err --last 200
./scripts/logs.sh --both --no-follow
~~~

## Uninstall

~~~
./scripts/uninstall.sh
./scripts/uninstall.sh --keep-logs
~~~

The uninstaller targets only this application's named plist, application
directory, and log directory. A configuration outside the application directory
selected with CC_AUTOMUX_CONFIG is not removed.
