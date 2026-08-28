#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

DIST_BIN="$REPO_ROOT/dist/cc-automux"
if [[ ! -x "$DIST_BIN" ]]; then
  cat >&2 <<EOF
Missing built binary:
  $DIST_BIN

Build it first:
  go build -trimpath -buildvcs=false -ldflags="-s -w" -o dist/cc-automux ./cmd/cc-automux
EOF
  exit 1
fi

CONFIG_EXISTED=0
[[ -f "$CONFIG_PATH" ]] && CONFIG_EXISTED=1

# The installer does not construct JSON or copy credentials into an
# environment. Initialization is delegated to the binary's shared config core,
# which prompts visibly on first creation and leaves an existing config/key
# untouched.
if (( CONFIG_EXISTED )); then
  "$DIST_BIN" init --config "$CONFIG_PATH"
else
  printf 'Generate a high-strength management key automatically? [y/N]: '
  if ! IFS= read -r generate_key; then
    echo "Could not read management key choice." >&2
    exit 1
  fi
  case "$generate_key" in
    y|Y|yes|YES|Yes)
      "$DIST_BIN" init --generate-management-key --config "$CONFIG_PATH"
      ;;
    *)
      "$DIST_BIN" init --config "$CONFIG_PATH"
      ;;
  esac
fi

# Only stop or replace the installed service after initialization has
# succeeded. A rejected key or invalid existing configuration therefore leaves
# the currently installed process untouched.
stop_launch_agent
mkdir -p "$BIN_DIR" "$LOG_DIR" "$PLIST_DIR"
install -m 755 "$DIST_BIN" "$BIN_PATH"
touch "$STDOUT_LOG" "$STDERR_LOG"
chmod 600 "$STDOUT_LOG" "$STDERR_LOG"
render_plist

start_launch_agent

cat <<EOF

Installed and started CC AutoMux.

Installed binary:
  $BIN_PATH

LaunchAgent:
  $PLIST_PATH

Config:
  $CONFIG_PATH

Logs:
  $STDOUT_LOG
  $STDERR_LOG
EOF
if (( CONFIG_EXISTED )); then
  cat <<EOF

Existing config kept unchanged:
  $CONFIG_PATH
EOF
else
  cat <<EOF

A new v1 configuration was initialized through cc-automux init.
Use the management API with the management key you just set.
EOF
fi

cat <<EOF

Management API:
  http://127.0.0.1:<configured-port>/api/v1/status

The service is loopback-only. Configure providers through:
  GET/PUT  /api/v1/config
  GET/POST /api/v1/providers
EOF

cat <<EOF

Useful commands:
  ./scripts/status.sh
  ./scripts/logs.sh
  ./scripts/stop.sh
  ./scripts/start.sh
  ./scripts/uninstall.sh
EOF
