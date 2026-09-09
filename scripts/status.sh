#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

resolve_installation "$BIN_PATH"
printf 'Config: %s\n' "$CONFIG_PATH"
service_status
"$BIN_PATH" check --config "$CONFIG_PATH" --ready --timeout 2s
