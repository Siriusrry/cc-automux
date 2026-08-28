#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

KEEP_LOGS=false
if [[ $# -eq 1 && "${1:-}" == "--keep-logs" ]]; then
  KEEP_LOGS=true
elif [[ $# -gt 0 ]]; then
  echo "Usage: ./scripts/uninstall.sh [--keep-logs]" >&2
  exit 1
fi

stop_launch_agent
# Move artifacts to the macOS Trash (recoverable via Finder "Put Back") instead
# of hard-deleting. Only the app's own named files/dirs are touched; the shared
# parent dirs (LaunchAgents, Application Support, Logs) are left intact.
# APP_DIR holds both the installed binary and config.json, so this removes both.
# Logs are removed by default; --keep-logs retains LOG_DIR.
trash "$PLIST_PATH"
trash "$APP_DIR"

if [[ "$KEEP_LOGS" == true ]]; then
  cat <<EOF
Uninstalled $LABEL (plist + app data moved to the Trash).

Logs were kept at:
  $LOG_DIR
EOF
else
  trash "$LOG_DIR"
  echo "Uninstalled $LABEL and moved plist, app data, and logs to the Trash."
fi
