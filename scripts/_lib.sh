#!/usr/bin/env bash

set -euo pipefail

LABEL="com.Siriusrry.cc-automux"
APP_NAME="cc-automux"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Platform selection. The binary plus Web UI form targets macOS and Linux; the
# desktop application is a separate distribution form and is not installed by
# these scripts.
case "$(uname -s)" in
  Darwin) PLATFORM="macos" ;;
  Linux) PLATFORM="linux" ;;
  *)
    echo "Unsupported platform: $(uname -s). The binary form targets macOS and Linux." >&2
    exit 1
    ;;
esac

# Per-platform locations. Each mirrors the path resolution compiled into the
# binary so the scripts and the process always agree on where state lives.
if [[ "$PLATFORM" == "macos" ]]; then
  APP_DIR="$HOME/Library/Application Support/$APP_NAME"
  DEFAULT_CONFIG_PATH="$APP_DIR/config.json"
  LOG_DIR="$HOME/Library/Logs/$APP_NAME"
  # macOS has no system-managed store for a service's stderr, so launchd is
  # pointed at a file. install.sh truncates it on every run: this channel only
  # carries fatal startup errors, which always need a human, and a previous
  # round's output has no diagnostic value for the current one. Truncating also
  # removes the unbounded growth this file would otherwise show while a
  # configuration stays fatally broken and the service keeps being restarted.
  BOOTSTRAP_LOG="$LOG_DIR/bootstrap.log"
  SERVICE_DIR="$HOME/Library/LaunchAgents"
  SERVICE_PATH="$SERVICE_DIR/$LABEL.plist"
  TEMPLATE_PATH="$REPO_ROOT/packaging/macos/launch-agent.plist.template"
  LAUNCH_DOMAIN="gui/$(id -u)"
  SERVICE_NAME="$LAUNCH_DOMAIN/$LABEL"
else
  # os.UserConfigDir honours XDG_CONFIG_HOME and otherwise uses ~/.config.
  _xdg_config="${XDG_CONFIG_HOME:-}"
  _xdg_config="${_xdg_config#"${_xdg_config%%[![:space:]]*}"}"
  _xdg_config="${_xdg_config%"${_xdg_config##*[![:space:]]}"}"
  if [[ -n "$_xdg_config" && "$_xdg_config" != /* ]]; then
    echo "XDG_CONFIG_HOME must be an absolute path, got: $_xdg_config" >&2
    exit 1
  fi
  _config_home="${_xdg_config:-$HOME/.config}"
  APP_DIR="$_config_home/$APP_NAME"
  DEFAULT_CONFIG_PATH="$APP_DIR/config.json"
  # The structured log lives in the XDG state directory, matching LogDir().
  _xdg_state="${XDG_STATE_HOME:-}"
  _xdg_state="${_xdg_state#"${_xdg_state%%[![:space:]]*}"}"
  _xdg_state="${_xdg_state%"${_xdg_state##*[![:space:]]}"}"
  if [[ -n "$_xdg_state" && "$_xdg_state" != /* ]]; then
    echo "XDG_STATE_HOME must be an absolute path, got: $_xdg_state" >&2
    exit 1
  fi
  LOG_DIR="${_xdg_state:-$HOME/.local/state}/$APP_NAME"
  # systemd collects the service's stderr into the journal, which has its own
  # capacity limit and rotation configured system-wide. There is no file to
  # create, point at, or trim, so no bootstrap path exists on this platform.
  BOOTSTRAP_LOG=""
  SERVICE_DIR="$_config_home/systemd/user"
  SERVICE_PATH="$SERVICE_DIR/$APP_NAME.service"
  TEMPLATE_PATH="$REPO_ROOT/packaging/linux/cc-automux.service.template"
  SERVICE_NAME="$APP_NAME.service"
  unset _xdg_config _xdg_state _config_home
fi

BIN_DIR="$APP_DIR/bin"
BIN_PATH="$BIN_DIR/$APP_NAME"
# Reported to the user by install.sh; the log files themselves are created and
# rotated by the process, not by these scripts.
# shellcheck disable=SC2034 # consumed by the scripts that source this file
ACTIVE_LOG="$LOG_DIR/$APP_NAME.log"
# shellcheck disable=SC2034 # consumed by the scripts that source this file
ARCHIVE_LOG="$LOG_DIR/$APP_NAME.log.1"

# Runtime config file. Mirrors the binary's v1 path resolution:
# CC_AUTOMUX_CONFIG must be an absolute path when set; otherwise the
# platform-standard location above is used. Leading/trailing whitespace is
# trimmed first, mirroring the binary's strings.TrimSpace, so a padded env value
# resolves identically.
CONFIG_PATH="$DEFAULT_CONFIG_PATH"
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
# Normalize the env var to the trimmed value so the rendered service carries the
# same path the binary would resolve. Empty when no override was set.
CC_AUTOMUX_CONFIG="$_cfg"
unset _cfg

xml_escape() {
  local value="$1"
  value=${value//&/&amp;}
  value=${value//</&lt;}
  value=${value//>/&gt;}
  value=${value//\"/&quot;}
  value=${value//\'/&apos;}
  printf '%s' "$value"
}

# unit_quote renders one value for a systemd directive that is unquoted on
# load, which ExecStart= and Environment= are. systemd splits those on
# whitespace and expands % specifiers, so a home directory containing a space
# or a percent sign would otherwise turn into a different program, different
# arguments or a different path. Inside double quotes systemd honours backslash
# escapes for the backslash and the quote, and a doubled percent sign stands
# for a literal one.
unit_quote() {
  local value="$1"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//%/%%}
  printf '"%s"' "$value"
}

# unit_env_quote renders one KEY=value assignment for Environment=. The whole
# assignment is quoted, which is the form systemd documents for values that
# contain whitespace.
unit_env_quote() {
  local name="$1" value="$2"
  unit_quote "$name=$value"
}

# unit_path renders one value for a single-path directive such as
# WorkingDirectory=. Those are not unquoted on load, so quotes would become
# part of the path and make it non-absolute; only leading and trailing
# whitespace is stripped, so interior spaces survive as they are. Specifiers
# are still expanded, so the percent sign is the one character to escape.
unit_path() {
  local value="$1"
  value=${value//%/%%}
  printf '%s' "$value"
}

require_template() {
  if [[ ! -f "$TEMPLATE_PATH" ]]; then
    echo "Missing service template: $TEMPLATE_PATH" >&2
    return 1
  fi
}

# render_service writes the per-user autostart registration for this platform.
# Registration is always per-user and never asks for elevation.
render_service() {
  if [[ "$PLATFORM" == "macos" ]]; then
    render_plist
  else
    render_systemd_unit
  fi
}

render_plist() {
  local rendered
  local config_env=""

  require_template

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
  rendered=${rendered//__BOOTSTRAP_LOG__/$(xml_escape "$BOOTSTRAP_LOG")}
  rendered=${rendered//__CONFIG_ENV__/$config_env}

  mkdir -p "$SERVICE_DIR"
  printf '%s\n' "$rendered" > "$SERVICE_PATH"
  chmod 600 "$SERVICE_PATH"
  plutil -lint "$SERVICE_PATH" >/dev/null
}

render_systemd_unit() {
  local rendered
  local config_env=""

  require_template

  if [[ -n "${CC_AUTOMUX_CONFIG:-}" ]]; then
    config_env="Environment=$(unit_env_quote CC_AUTOMUX_CONFIG "$CONFIG_PATH")"
  fi

  rendered="$(<"$TEMPLATE_PATH")"
  rendered=${rendered//__BINARY_PATH__/$(unit_quote "$BIN_PATH")}
  rendered=${rendered//__WORKING_DIRECTORY__/$(unit_path "$APP_DIR")}
  rendered=${rendered//__CONFIG_ENV__/$config_env}

  mkdir -p "$SERVICE_DIR"
  printf '%s\n' "$rendered" > "$SERVICE_PATH"
  chmod 600 "$SERVICE_PATH"
  systemctl --user daemon-reload
}

start_service() {
  if [[ ! -f "$SERVICE_PATH" ]]; then
    echo "Service is not installed: $SERVICE_PATH" >&2
    echo "Run ./scripts/install.sh first." >&2
    return 1
  fi
  if [[ "$PLATFORM" == "macos" ]]; then
    launchctl bootstrap "$LAUNCH_DOMAIN" "$SERVICE_PATH" 2>/dev/null || true
    launchctl kickstart -k "$SERVICE_NAME"
  else
    # enable registers the per-user autostart; restart covers both a first start
    # and replacing an already running instance.
    systemctl --user enable "$SERVICE_NAME" >/dev/null
    systemctl --user restart "$SERVICE_NAME"
  fi
}

stop_service() {
  if [[ "$PLATFORM" == "macos" ]]; then
    if [[ -f "$SERVICE_PATH" ]]; then
      launchctl bootout "$LAUNCH_DOMAIN" "$SERVICE_PATH" 2>/dev/null || true
    else
      launchctl bootout "$SERVICE_NAME" 2>/dev/null || true
    fi
  else
    systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
  fi
}

# unregister_service removes the autostart registration. On Linux the unit file
# has to stay in place for disable to resolve the name, so the caller deletes it
# afterwards and then calls forget_service so systemd drops the unit it still
# holds in memory.
unregister_service() {
  stop_service
  if [[ "$PLATFORM" == "linux" ]]; then
    systemctl --user disable "$SERVICE_NAME" >/dev/null 2>&1 || true
  fi
}

# forget_service is called after the registration file has been deleted. launchd
# forgets a booted-out agent on its own; systemd keeps a loaded unit until it is
# told to reload, and would otherwise keep reporting the deleted unit.
forget_service() {
  if [[ "$PLATFORM" == "linux" ]]; then
    systemctl --user daemon-reload 2>/dev/null || true
    systemctl --user reset-failed "$SERVICE_NAME" 2>/dev/null || true
  fi
}

service_status() {
  if [[ "$PLATFORM" == "macos" ]]; then
    launchctl print "$SERVICE_NAME"
  else
    systemctl --user status "$SERVICE_NAME" --no-pager
  fi
}

# remove_path deletes an installed artifact. On macOS it goes to the Trash via
# Finder so removed items keep "Put Back" support; when Finder/osascript is
# unavailable (headless/SSH/no GUI session) the delete is detected as failed and
# falls back to a permanent rm -rf with a warning, so uninstall always completes
# instead of aborting mid-sequence under set -e. Linux offers no equivalent
# user-level Trash guarantee for arbitrary paths, so removal there is permanent.
remove_path() {
  local target="$1"
  [[ -e "$target" ]] || return 0
  if [[ "$PLATFORM" != "macos" ]]; then
    rm -rf "$target"
    return 0
  fi
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

# describe_fallback prints where fatal startup errors go on this platform. The
# structured log itself is always read through the management API.
describe_fallback() {
  if [[ "$PLATFORM" == "macos" ]]; then
    printf '%s\n' "  $BOOTSTRAP_LOG (truncated on each install)"
  else
    printf '%s\n' "  journalctl --user -u $SERVICE_NAME"
  fi
}
