#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

require_manager
resolve_installation "$BIN_PATH"
umask 077
acquire_install_lock
trap 'rmdir "$INSTALL_LOCK"' EXIT
stop_service

echo "Stopped $SERVICE_NAME"
