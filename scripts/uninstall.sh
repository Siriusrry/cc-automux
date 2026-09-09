#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

uninstall_main() {
KEEP_LOGS=false
if [[ $# -eq 1 && "${1:-}" == "--keep-logs" ]]; then
  KEEP_LOGS=true
elif [[ $# -gt 0 ]]; then
  echo "Usage: ./scripts/uninstall.sh [--keep-logs]" >&2
  exit 1
fi

require_manager
resolve_installation "$BIN_PATH"
umask 077
acquire_install_lock
trap 'rmdir "$INSTALL_LOCK"' EXIT

# Stop the service and drop the autostart registration before deleting anything.
# On Linux the unit file must still exist for disable to resolve the name.
unregister_service
# Only the app's own named files/dirs are touched; the shared parent dirs
# (LaunchAgents, systemd/user, Application Support, Logs, .config, .local/state)
# are left intact. APP_DIR holds both the installed binary and config.json, so
# this removes both. Logs are removed by default; --keep-logs retains LOG_DIR.
# On macOS these move to the Trash and stay recoverable via Finder "Put Back";
# on Linux the removal is permanent.
remove_path "$SERVICE_PATH"
forget_service
remove_path "$APP_DIR"

if [[ "$KEEP_LOGS" == true ]]; then
  cat <<EOF
Uninstalled $SERVICE_NAME (autostart registration and app data removed).

Logs were kept at:
  $LOG_DIR
EOF
else
  remove_path "$LOG_DIR"
  echo "Uninstalled $SERVICE_NAME and removed the autostart registration, app data, and logs."
fi
echo 'Claude Code settings were not restored. Any custom configuration outside the application directory was kept.'
}

uninstall_main "$@"
