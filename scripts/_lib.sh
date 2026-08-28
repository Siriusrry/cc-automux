#!/usr/bin/env bash

set -euo pipefail

LABEL="com.Siriusrry.cc-automux"
APP_NAME="cc-automux"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_DIR="$HOME/Library/Application Support/$APP_NAME"
BIN_DIR="$APP_DIR/bin"
BIN_PATH="$BIN_DIR/$APP_NAME"
# Runtime config file. Mirrors the binary's v1 path resolution:
# CC_AUTOMUX_CONFIG must be an absolute path when set; otherwise the macOS
# platform-standard location is APP_DIR/config.json. Leading/trailing
# whitespace is trimmed first, mirroring the binary's strings.TrimSpace, so a
# padded env value resolves identically.
CONFIG_PATH="$APP_DIR/config.json"
_cfg="${CC_AUTOMUX_CONFIG:-}"
_cfg="${_cfg#"${_cfg%%[![:space:]]*}"}"
_cfg="${_cfg%"${_cfg##*[![:space:]]}"}"
if [[ -n "$_cfg" ]]; then
  if [[ "$_cfg" != /* ]]; then
    echo "CC_AUTOMUX_CONFIG must be an absolute path, got: $_cfg" >&2
    exit 1
  fi
  CONFIG_PATH="$_cfg"
fi
# Normalize the env var to the trimmed value so render_plist writes the same
# path the binary would resolve. Empty when no override was set.
CC_AUTOMUX_CONFIG="$_cfg"
unset _cfg
LOG_DIR="$HOME/Library/Logs/$APP_NAME"
STDOUT_LOG="$LOG_DIR/stdout.log"
STDERR_LOG="$LOG_DIR/stderr.log"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST_PATH="$PLIST_DIR/$LABEL.plist"
TEMPLATE_PATH="$REPO_ROOT/packaging/macos/launch-agent.plist.template"
LAUNCH_DOMAIN="gui/$(id -u)"
SERVICE_NAME="$LAUNCH_DOMAIN/$LABEL"
xml_escape() {
  local value="$1"
  value=${value//&/&amp;}
  value=${value//</&lt;}
  value=${value//>/&gt;}
  value=${value//\"/&quot;}
  value=${value//\'/&apos;}
  printf '%s' "$value"
}

render_plist() {
  local rendered
  local config_env=""

  if [[ ! -f "$TEMPLATE_PATH" ]]; then
    echo "Missing launchd template: $TEMPLATE_PATH" >&2
    return 1
  fi

  # Only carry CC_AUTOMUX_CONFIG into the LaunchAgent env when an override was
  # set at install time; otherwise the binary uses its own default path and the
  # placeholder collapses to nothing.
  if [[ -n "${CC_AUTOMUX_CONFIG:-}" ]]; then
    config_env="
    <key>CC_AUTOMUX_CONFIG</key>
    <string>$(xml_escape "$CONFIG_PATH")</string>
"
  fi

  rendered="$(<"$TEMPLATE_PATH")"
  rendered=${rendered//__LABEL__/$(xml_escape "$LABEL")}
  rendered=${rendered//__BINARY_PATH__/$(xml_escape "$BIN_PATH")}
  rendered=${rendered//__WORKING_DIRECTORY__/$(xml_escape "$APP_DIR")}
  rendered=${rendered//__STDOUT_LOG__/$(xml_escape "$STDOUT_LOG")}
  rendered=${rendered//__STDERR_LOG__/$(xml_escape "$STDERR_LOG")}
  rendered=${rendered//__CONFIG_ENV__/$config_env}

  mkdir -p "$PLIST_DIR"
  printf '%s\n' "$rendered" > "$PLIST_PATH"
  chmod 600 "$PLIST_PATH"
  plutil -lint "$PLIST_PATH" >/dev/null
}

start_launch_agent() {
  if [[ ! -f "$PLIST_PATH" ]]; then
    echo "LaunchAgent is not installed: $PLIST_PATH" >&2
    echo "Run ./scripts/install.sh first." >&2
    return 1
  fi
  launchctl bootstrap "$LAUNCH_DOMAIN" "$PLIST_PATH" 2>/dev/null || true
  launchctl kickstart -k "$SERVICE_NAME"
}

stop_launch_agent() {
  if [[ -f "$PLIST_PATH" ]]; then
    launchctl bootout "$LAUNCH_DOMAIN" "$PLIST_PATH" 2>/dev/null || true
  else
    launchctl bootout "$SERVICE_NAME" 2>/dev/null || true
  fi
}

# Move a file or directory to the macOS Trash via Finder, so removed items keep
# "Put Back" support instead of being unrecoverably deleted. No-op when the
# target is absent. Finder resolves Trash name collisions on its own. When
# Finder/osascript is unavailable (headless/SSH/no GUI session) the delete is
# detected as failed and we fall back to a permanent rm -rf with a warning, so
# uninstall always completes instead of aborting mid-sequence under set -e.
trash() {
  local target="$1"
  [[ -e "$target" ]] || return 0
  # Escape backslash first, then double quote, before interpolating the path
  # into the AppleScript string literal.
  local escaped="$target"
  escaped=${escaped//\\/\\\\}
  escaped=${escaped//\"/\\\"}
  if osascript -e "tell application \"Finder\" to delete (POSIX file \"$escaped\" as alias)" >/dev/null 2>&1; then
    return 0
  fi
  echo "Warning: could not move to Trash via Finder (no GUI session?); removing permanently: $target" >&2
  rm -rf "$target"
}
