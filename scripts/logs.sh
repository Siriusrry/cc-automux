#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"

MODE="both"
LAST="100"
FOLLOW=true

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      MODE="out"
      shift
      ;;
    --err)
      MODE="err"
      shift
      ;;
    --both)
      MODE="both"
      shift
      ;;
    --last)
      if [[ $# -lt 2 ]]; then
        echo "--last requires a line count" >&2
        exit 1
      fi
      LAST="$2"
      if [[ ! "$LAST" =~ ^[0-9]+$ ]] || [[ "$LAST" == "0" ]]; then
        echo "--last must be a positive integer, got: $LAST" >&2
        exit 1
      fi
      shift 2
      ;;
    --no-follow)
      FOLLOW=false
      shift
      ;;
    -h|--help)
      cat <<EOF
Usage: ./scripts/logs.sh [--out|--err|--both] [--last N] [--no-follow]

Default: show stdout.log and stderr.log, last 100 lines, and follow new entries.

  --out        show normal/success log only
  --err        show error/diagnostic log only
  --both       show both logs (default)
  --last N     show the last N lines (default: 100)
  --no-follow  print once instead of tailing live logs
EOF
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      echo "Run ./scripts/logs.sh --help" >&2
      exit 1
      ;;
  esac
done

mkdir -p "$LOG_DIR"
touch "$STDOUT_LOG" "$STDERR_LOG"

files=()
case "$MODE" in
  out)
    files=("$STDOUT_LOG")
    ;;
  err)
    files=("$STDERR_LOG")
    ;;
  both)
    files=("$STDOUT_LOG" "$STDERR_LOG")
    ;;
esac

if [[ "$FOLLOW" == true ]]; then
  exec tail -n "$LAST" -F "${files[@]}"
else
  exec tail -n "$LAST" "${files[@]}"
fi
