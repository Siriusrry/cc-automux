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
  # pointed at a file. Installation replacement truncates it: this channel only
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

die() { echo "CC AutoMux: $*" >&2; exit 1; }

refresh_paths() {
  BIN_DIR="$APP_DIR/bin"
  BIN_PATH="$BIN_DIR/$APP_NAME"
  DEFAULT_CONFIG_PATH="$APP_DIR/config.json"
  SERVICE_DIR="$(dirname "$SERVICE_PATH")"
  # shellcheck disable=SC2034 # reported by consumers
  ACTIVE_LOG="$LOG_DIR/$APP_NAME.log"
  # shellcheck disable=SC2034 # reported by consumers
  ARCHIVE_LOG="$LOG_DIR/$APP_NAME.log.1"
  if [[ "$PLATFORM" == macos ]]; then BOOTSTRAP_LOG="$LOG_DIR/bootstrap.log"; fi
}

read_install_paths() {
  local record="$APP_DIR/install-paths" values=() value
  [[ -f "$record" && ! -L "$record" ]] || return 1
  while IFS= read -r value || [[ -n "$value" ]]; do values+=("$value"); done < "$record"
  [[ ${#values[@]} -eq 3 ]] || die "Invalid installation path record: $record"
  for value in "${values[@]}"; do
    [[ "$value" == /* && "$value" != *$'\n'* && "$value" != *$'\r'* && "/$value/" != */../* ]] || die "Invalid installed path."
  done
  CONFIG_PATH="${values[0]}" LOG_DIR="${values[1]}" SERVICE_PATH="${values[2]}"
  [[ "$(basename "$LOG_DIR")" == "$APP_NAME" ]] || die "Invalid application log path."
  if [[ "$PLATFORM" == macos ]]; then
    [[ "$(basename "$SERVICE_PATH")" == "$LABEL.plist" ]] || die "Invalid service path."
  else
    [[ "$(basename "$SERVICE_PATH")" == "$APP_NAME.service" ]] || die "Invalid service path."
  fi
}

# Resolve an existing registration before considering the caller's environment.
# Installed scripts have their own path record, so they survive download cleanup
# and a change in the terminal's XDG or CC_AUTOMUX_CONFIG values.
resolve_installation() {
  local checker="$1" registered="" details values=() value
  if [[ -f "$REPO_ROOT/install-paths" ]]; then
    APP_DIR="$REPO_ROOT"
    read_install_paths
  else
    if [[ "$PLATFORM" == linux ]]; then
      registered="$(systemctl --user show "$SERVICE_NAME" --property=FragmentPath --value 2>/dev/null || true)"
      if [[ -n "$registered" ]]; then
        [[ "$registered" == /* ]] || die "Cannot resolve the existing service registration."
        if [[ -f "$SERVICE_PATH" && "$SERVICE_PATH" != "$registered" ]]; then die "Multiple service registrations found; resolve them before installing."; fi
        SERVICE_PATH="$registered"
      fi
    fi
    if [[ -f "$SERVICE_PATH" ]]; then
      [[ -x "$checker" ]] || die "An executable binary is required to inspect the existing service."
      details="$("$checker" inspect-service "$SERVICE_PATH")" || die "Cannot safely inspect the existing registration."
      while IFS= read -r value; do values+=("$value"); done <<< "$details"
      [[ ${#values[@]} -eq 3 ]] || die "Invalid service inspection result."
      APP_DIR="${values[0]}" CONFIG_PATH="${values[1]}"
      if [[ "$PLATFORM" == linux ]]; then LOG_DIR="${values[2]}/$APP_NAME"; fi
      if [[ -f "$APP_DIR/install-paths" ]]; then
        read_install_paths
        [[ "$CONFIG_PATH" == "${values[1]}" ]] || die "The service configuration path differs from its installation record; reconcile them before upgrading."
      fi
    elif [[ -f "$APP_DIR/install-paths" ]]; then
      read_install_paths
    fi
  fi
  [[ "$(basename "$APP_DIR")" == "$APP_NAME" && ! -L "$APP_DIR" ]] || die "Invalid application directory."
  refresh_paths
  CC_AUTOMUX_CONFIG="$CONFIG_PATH"
  export CC_AUTOMUX_CONFIG
  if [[ "$PLATFORM" == linux ]]; then XDG_STATE_HOME="$(dirname "$LOG_DIR")"; export XDG_STATE_HOME; fi
}

require_manager() {
  if [[ "$PLATFORM" == linux ]]; then
    if ! command -v systemctl >/dev/null || ! systemctl --user show-environment >/dev/null 2>&1; then
      die "Linux requires an available systemd user manager in a logged-in user session."
    fi
  else
    launchctl print "$LAUNCH_DOMAIN" >/dev/null 2>&1 || die "macOS requires a logged-in graphical user session."
  fi
}

acquire_install_lock() {
  INSTALL_LOCK="$HOME/.cc-automux-install.lock"
  mkdir "$INSTALL_LOCK" 2>/dev/null || die "Another operation is running or an interrupted operation left $INSTALL_LOCK; resolve it before retrying."
}

service_running() {
  if [[ "$PLATFORM" == macos ]]; then
    launchctl print "$SERVICE_NAME" 2>/dev/null | grep -q 'state = running'
  else
    systemctl --user is-active --quiet "$SERVICE_NAME"
  fi
}

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

unit_exec_quote() {
  local value="$1"
  value=${value//\$/\$\$}
  unit_quote "$value"
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

  # Installation pins the resolved path; standalone template rendering can
  # still omit an override and let the binary use its default path.
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
  rendered=${rendered//__BINARY_PATH__/$(unit_exec_quote "$BIN_PATH")}
  rendered=${rendered//__WORKING_DIRECTORY__/$(unit_path "$APP_DIR")}
  rendered=${rendered//__CONFIG_ENV__/$config_env}
  if [[ -n "${XDG_STATE_HOME:-}" ]]; then
    rendered+=$'\n'
    # Append within [Service], before [Install], so the log directory is pinned.
    rendered=${rendered/\[Install\]/Environment=$(unit_env_quote XDG_STATE_HOME "$XDG_STATE_HOME")$'\n\n'[Install]}
  fi

  mkdir -p "$SERVICE_DIR"
  printf '%s\n' "$rendered" > "$SERVICE_PATH"
  chmod 600 "$SERVICE_PATH"
  if [[ "${RENDER_ONLY:-0}" != 1 ]]; then systemctl --user daemon-reload; fi
}

start_service() {
  if [[ ! -f "$SERVICE_PATH" ]]; then
    echo "Service is not installed: $SERVICE_PATH" >&2
    echo "Run ./scripts/install.sh first." >&2
    return 1
  fi
  if [[ "$PLATFORM" == "macos" ]]; then
    launchctl enable "$SERVICE_NAME"
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
    if launchctl print "$SERVICE_NAME" >/dev/null 2>&1; then launchctl bootout "$SERVICE_NAME"; fi
  else
    systemctl --user stop "$SERVICE_NAME" 2>/dev/null || { [[ ! -f "$SERVICE_PATH" ]] || return 1; }
  fi
}

# unregister_service removes the autostart registration. On Linux the unit file
# has to stay in place for disable to resolve the name, so the caller deletes it
# afterwards and then calls forget_service so systemd drops the unit it still
# holds in memory.
unregister_service() {
  stop_service
  if [[ "$PLATFORM" == "linux" ]]; then
    systemctl --user disable "$SERVICE_NAME" >/dev/null 2>&1 || { [[ ! -f "$SERVICE_PATH" ]] || return 1; }
  fi
}

# forget_service is called after the registration file has been deleted. launchd
# forgets a booted-out agent on its own; systemd keeps a loaded unit until it is
# told to reload, and would otherwise keep reporting the deleted unit.
forget_service() {
  if [[ "$PLATFORM" == "linux" ]]; then
    systemctl --user daemon-reload
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
    printf '%s\n' "  $BOOTSTRAP_LOG (truncated when replacing the installation)"
  else
    printf '%s\n' "  journalctl --user -u $SERVICE_NAME"
  fi
}
