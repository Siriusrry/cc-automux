#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

# Non-interactive install. Each setting is taken from an env var, falling back
# to its default; override any subset on (re)install, e.g.:
#   PORT=9000 CLIPROXY_UPSTREAM=https://127.0.0.1:8317 LOG_MAX_MB=200 ./scripts/install.sh
# These three feed the LaunchAgent bootstrap env that render_plist writes
# (CC_AUTO_SHIM_LISTEN / CC_CLIPROXY_SHIM_UPSTREAM / CC_AUTO_SHIM_LOG_MAX_BYTES)
# and only seed config.json on its first launch; once config.json exists the
# file wins and these are ignored.
PORT="${PORT:-$DEFAULT_PORT}"
validate_port "$PORT"

CLIPROXY_UPSTREAM="${CLIPROXY_UPSTREAM:-$DEFAULT_CLIPROXY_UPSTREAM}"
validate_upstream "$CLIPROXY_UPSTREAM"

LOG_MAX_MB="${LOG_MAX_MB:-$DEFAULT_LOG_MAX_MB}"
validate_log_max_mb "$LOG_MAX_MB"
LOG_MAX_BYTES="$(mb_to_bytes "$LOG_MAX_MB")"

LISTEN_ADDR="127.0.0.1:$PORT"
DIST_BIN="$REPO_ROOT/dist/cc-auto-mode-shim"

if [[ ! -x "$DIST_BIN" ]]; then
  cat >&2 <<EOF
Missing built binary:
  $DIST_BIN

Build it first:
  go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-auto-mode-shim ./cmd/cc-auto-mode-shim
EOF
  exit 1
fi

mkdir -p "$BIN_DIR" "$LOG_DIR" "$PLIST_DIR"
install -m 755 "$DIST_BIN" "$BIN_PATH"
touch "$STDOUT_LOG" "$STDERR_LOG"
chmod 644 "$STDOUT_LOG" "$STDERR_LOG"

# The binary self-seeds config.json on first launch from the LaunchAgent env
# rendered below (CC_AUTO_SHIM_LISTEN / CC_CLIPROXY_SHIM_UPSTREAM); the script
# does not write the config itself. Record whether a persisted config already
# exists (before the service starts) so the messaging below is accurate.
CONFIG_EXISTED=0
[[ -f "$CONFIG_PATH" ]] && CONFIG_EXISTED=1

render_plist "$LISTEN_ADDR" "$CLIPROXY_UPSTREAM" "$LOG_MAX_BYTES"
stop_launch_agent
start_launch_agent

cat <<EOF

Installed and started cc-auto-mode-shim.

Installed binary:
  $BIN_PATH

LaunchAgent:
  $PLIST_PATH

Config:
  $CONFIG_PATH

Logs:
  $STDOUT_LOG
  $STDERR_LOG
  max size: ${LOG_MAX_MB} MB each
EOF

if (( CONFIG_EXISTED )); then
  cat <<EOF

Existing config kept:
  $CONFIG_PATH

Install only refreshed the binary, the LaunchAgent, and its bootstrap env. The
existing config file stays authoritative for listen_addr and upstreams; if its
port differs from $PORT, use that port for the URLs below.
EOF
else
  cat <<EOF

No config existed, so the shim seeds it from the values above on first launch:
  $CONFIG_PATH
EOF
  print_base_urls "$PORT"
fi

cat <<EOF

Configure the shim in the web config desk:
  http://127.0.0.1:$PORT/admin

Most settings (accounts, keys, routing, classifier target, enable switch) apply
live from /admin without a restart. Changing the listen port or the max log size
from /admin now restarts the shim automatically (in place, same PID) to apply it.
Re-running install.sh is only needed to change the LaunchAgent bootstrap env or
to reinstall the binary.

Reinstall non-interactively (override any subset; unset values keep defaults):
  PORT=$PORT CLIPROXY_UPSTREAM=$CLIPROXY_UPSTREAM LOG_MAX_MB=$LOG_MAX_MB ./scripts/install.sh

First time? Open http://127.0.0.1:$PORT/admin and add your AnyRouter account(s)
and/or CPA key before pointing Claude Code at the shim.
EOF

cat <<EOF

Useful commands:
  ./scripts/status.sh
  ./scripts/logs.sh
  ./scripts/stop.sh
  ./scripts/start.sh
  ./scripts/uninstall.sh
EOF
