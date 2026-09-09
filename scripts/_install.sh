#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/_lib.sh
source "$SCRIPT_DIR/_lib.sh"

SOURCE="${1:-}" TARGET_VERSION="${2:-}"
[[ "$SOURCE" == local || ( "$SOURCE" == public && -n "$TARGET_VERSION" ) ]] || die "Use install.sh [--local]."
DIST_BIN="$REPO_ROOT/dist/cc-automux"
[[ -f "$DIST_BIN" && -x "$DIST_BIN" && ! -L "$DIST_BIN" ]] || die "Missing executable dist/cc-automux; build it before using --local."
case "$(uname -m)" in
  x86_64|amd64) EXPECTED_ARCH=amd64 ;;
  arm64|aarch64) EXPECTED_ARCH=arm64 ;;
  *) die "Unsupported CPU architecture." ;;
esac
EXPECTED_OS=linux
[[ "$PLATFORM" != macos ]] || EXPECTED_OS=darwin
[[ "$("$DIST_BIN" --platform)" == "$EXPECTED_OS/$EXPECTED_ARCH" ]] || die "Local binary does not match this platform."
NEW_DISPLAY="$("$DIST_BIN" --version)"
[[ "$NEW_DISPLAY" == 'CC AutoMux v'* ]] || die "Unrecognized candidate binary."
NEW_VERSION="${NEW_DISPLAY#CC AutoMux }"
"$DIST_BIN" compare-version "$NEW_VERSION" >/dev/null || die "Unrecognized candidate version."
[[ "$SOURCE" != public || "$NEW_VERSION" == "$TARGET_VERSION" ]] || die "Release version mismatch."

require_manager
resolve_installation "$DIST_BIN"
acquire_install_lock
TX="" STOPPED=0 REPLACED=0 WAS_RUNNING=0 WAS_ENABLED=0 HAD_SERVICE=0
OLD_VERSION="" OLD_SOURCE="" CONFIG_EXISTED=0 KEEP_BACKUP=0
PARTS=(bin scripts packaging docs README.md README.zh-CN.md LICENSE install-paths)

rollback_install() {
  local part
  echo "Installation failed; restoring the previous program and service registration." >&2
  stop_service || return 1
  if (( REPLACED )); then
    for part in "${PARTS[@]}"; do
      rm -rf "${APP_DIR:?}/$part" || return 1
      if [[ -e "$TX/old/$part" ]]; then mv "$TX/old/$part" "$APP_DIR/$part" || return 1; fi
    done
    if [[ -f "$TX/old/install-source" ]]; then
      cp -p "$TX/old/install-source" "$APP_DIR/install-source" || return 1
    else
      rm -f "$APP_DIR/install-source" || return 1
    fi
  fi
  if (( HAD_SERVICE )); then
    cp -p "$TX/service.old" "$SERVICE_PATH.rollback" || return 1
    mv "$SERVICE_PATH.rollback" "$SERVICE_PATH" || return 1
  else
    if [[ "$PLATFORM" == linux ]]; then systemctl --user disable "$SERVICE_NAME" >/dev/null 2>&1 || true; fi
    rm -f "$SERVICE_PATH" || return 1
  fi
  if [[ "$PLATFORM" == linux ]]; then
    systemctl --user daemon-reload || return 1
    if (( HAD_SERVICE && ! WAS_ENABLED )); then systemctl --user disable "$SERVICE_NAME" >/dev/null || return 1; fi
  fi
  if (( WAS_RUNNING )); then
    start_service || return 1
    if [[ "$PLATFORM" == linux && "$WAS_ENABLED" == 0 ]]; then systemctl --user disable "$SERVICE_NAME" >/dev/null || return 1; fi
    "$DIST_BIN" check --config "$CONFIG_PATH" --ready --expect-version "$OLD_VERSION" --timeout 30s >/dev/null || return 1
  fi
  echo "Previous installation restored; configuration was kept." >&2
}

finish_install() {
  local result=$?
  trap - EXIT INT TERM
  if (( result != 0 )); then
    if (( STOPPED )); then
      if ! rollback_install; then
        KEEP_BACKUP=1
        echo "Automatic recovery could not complete. Recovery files retained at: $TX" >&2
      fi
    fi
    echo "Check: $APP_DIR/scripts/status.sh" >&2
    describe_fallback >&2
  fi
  if [[ -n "$TX" && "$KEEP_BACKUP" == 0 ]]; then rm -rf "$TX"; fi
  rmdir "$INSTALL_LOCK" || true
  exit "$result"
}
trap finish_install EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for PART in "${PARTS[@]}" install-source; do
  case "$CONFIG_PATH/" in "$APP_DIR/$PART/"*) die "Configuration overlaps installed program files." ;; esac
  case "$LOG_DIR/" in "$APP_DIR/$PART/"*) die "Log directory overlaps installed program files." ;; esac
done
[[ "$CONFIG_PATH" != "$SERVICE_PATH" && "$CONFIG_PATH" != "$BOOTSTRAP_LOG" ]] || die "Configuration overlaps service files."

if [[ -e "$APP_DIR/install-source" ]]; then
  [[ -f "$APP_DIR/install-source" && ! -L "$APP_DIR/install-source" ]] || die "Invalid installation source record."
  OLD_SOURCE="$(cat "$APP_DIR/install-source")"
  [[ "$OLD_SOURCE" == public || "$OLD_SOURCE" == local ]] || die "Unknown installation source."
fi
if [[ -e "$BIN_PATH" || -L "$BIN_PATH" ]]; then
  [[ -x "$BIN_PATH" && -f "$BIN_PATH" && ! -L "$BIN_PATH" ]] || die "Installed binary is not a regular executable."
  OLD_DISPLAY="$("$BIN_PATH" --version)" || die "Cannot determine installed version."
  [[ "$OLD_DISPLAY" == 'CC AutoMux v'* ]] || die "Cannot determine installed product/version."
  OLD_VERSION="${OLD_DISPLAY#CC AutoMux }"
  COMPARED="$("$DIST_BIN" compare-version "$OLD_VERSION")" || die "Cannot compare installed version."
  [[ "$OLD_VERSION" != v0.* ]] || die "v0 installations require manual removal; no migration is provided."
  if [[ "$SOURCE" == public ]]; then
    [[ "$COMPARED" != -1 ]] || die "Refusing downgrade from $OLD_VERSION to $NEW_VERSION."
    [[ "$COMPARED" != 0 || "$OLD_SOURCE" == public ]] || die "Refusing same-version replacement of a local or unmarked build; resolve it manually."
  fi
elif [[ "$SOURCE" == public && "$OLD_SOURCE" == local ]]; then
  die "Local installation has no readable binary version; resolve it manually."
fi

[[ ! -e "$CONFIG_PATH.pending" && ! -L "$CONFIG_PATH.pending" ]] || die "Unfinished configuration restart; resolve the pending file first."
if [[ -e "$CONFIG_PATH" || -L "$CONFIG_PATH" ]]; then
  CONFIG_EXISTED=1
  UI_URL="$("$DIST_BIN" check --config "$CONFIG_PATH")" || die "The selected binary does not support the existing configuration; it was left unchanged."
else
  [[ -z "$OLD_VERSION" && -z "$OLD_SOURCE" && ! -f "$SERVICE_PATH" ]] || die "An existing installation has lost its configuration; refusing to reset it."
  if ! { exec 3<>/dev/tty; } 2>/dev/null; then die "First installation requires a controlling terminal for the port and management key prompts."; fi
  while true; do
    printf 'Listen port [8765]: ' >&3
    IFS= read -r PORT <&3 || die "Could not read the port."
    PORT="${PORT:-8765}"
    if [[ ! "$PORT" =~ ^[0-9]{1,5}$ ]] || (( 10#$PORT < 1 || 10#$PORT > 65535 )); then
      echo "Enter a port from 1 to 65535." >&3; continue
    fi
    PORT="$((10#$PORT))"
    if "$DIST_BIN" check --listen-addr "127.0.0.1:$PORT" >&3 2>&3; then break; fi
    echo "Select another port." >&3
  done
  printf 'Generate a high-strength management key automatically? [Y/n]: ' >&3
  IFS= read -r GENERATE <&3 || die "Could not read the management key choice."
  case "$GENERATE" in
    ''|y|Y|yes|YES|Yes) INIT_FLAGS=(--generate-management-key) ;;
    n|N|no|NO|No) INIT_FLAGS=() ;;
    *) die "Choose yes or no, then run the installer again." ;;
  esac
  "$DIST_BIN" init --config "$CONFIG_PATH" --listen-addr "127.0.0.1:$PORT" ${INIT_FLAGS[@]+"${INIT_FLAGS[@]}"} <&3
  exec 3>&-
  UI_URL="$("$DIST_BIN" check --config "$CONFIG_PATH")"
fi

mkdir -p "$APP_DIR" "$LOG_DIR" "$SERVICE_DIR"
TX="$(mktemp -d "$APP_DIR/.install-transaction.XXXXXX")"
mkdir "$TX/next" "$TX/old"
mkdir -p "$TX/next/bin" "$TX/next/scripts" "$TX/next/packaging" "$TX/next/docs"
install -m 755 "$DIST_BIN" "$TX/next/bin/cc-automux"
for SCRIPT in "$REPO_ROOT"/scripts/*.sh; do install -m 755 "$SCRIPT" "$TX/next/scripts/"; done
cp "$REPO_ROOT/scripts/README.md" "$REPO_ROOT/scripts/README.zh-CN.md" "$TX/next/scripts/"
cp -R "$REPO_ROOT/packaging/$PLATFORM" "$TX/next/packaging/"
cp "$REPO_ROOT/docs/usage.md" "$REPO_ROOT/docs/usage.zh-CN.md" "$TX/next/docs/"
cp "$REPO_ROOT/README.md" "$REPO_ROOT/README.zh-CN.md" "$REPO_ROOT/LICENSE" "$TX/next/"
printf '%s\n%s\n%s\n' "$CONFIG_PATH" "$LOG_DIR" "$SERVICE_PATH" > "$TX/next/install-paths"
# Render and validate before stopping the existing service.
ORIGINAL_SERVICE_PATH="$SERVICE_PATH" ORIGINAL_SERVICE_DIR="$SERVICE_DIR"
SERVICE_PATH="$TX/service.new" SERVICE_DIR="$TX" RENDER_ONLY=1
render_service
SERVICE_PATH="$ORIGINAL_SERVICE_PATH" SERVICE_DIR="$ORIGINAL_SERVICE_DIR"
unset RENDER_ONLY

COMPLETE=1
for PART in "${PARTS[@]}"; do
  if [[ ! -e "$APP_DIR/$PART" ]] || ! diff -qr "$APP_DIR/$PART" "$TX/next/$PART" >/dev/null 2>&1; then COMPLETE=0; fi
done
if [[ ! -f "$SERVICE_PATH" ]] || ! cmp -s "$SERVICE_PATH" "$TX/service.new"; then COMPLETE=0; fi
if [[ "$SOURCE" == public && "$OLD_VERSION" == "$NEW_VERSION" && "$COMPLETE" == 1 ]]; then
  "$DIST_BIN" check --config "$CONFIG_PATH" --ready --timeout 5s >/dev/null || die "Already the latest version, but the service is not ready."
  printf 'Already up to date: CC AutoMux %s\nConfig: %s\nWeb UI: %s\n' "$NEW_VERSION" "$CONFIG_PATH" "$UI_URL"
  exit 0
fi

for PART in "${PARTS[@]}" install-source; do
  if [[ -e "$APP_DIR/$PART" || -L "$APP_DIR/$PART" ]]; then
    [[ ! -L "$APP_DIR/$PART" ]] || die "Refusing to replace a symbolic installation path: $PART"
    cp -pR "$APP_DIR/$PART" "$TX/old/"
  fi
done
if [[ -f "$SERVICE_PATH" ]]; then
  [[ ! -L "$SERVICE_PATH" ]] || die "Service registration must not be a symbolic link."
  HAD_SERVICE=1
  cp -p "$SERVICE_PATH" "$TX/service.old"
fi
if service_running; then WAS_RUNNING=1; fi
if [[ "$PLATFORM" == linux ]] && systemctl --user is-enabled --quiet "$SERVICE_NAME"; then WAS_ENABLED=1; fi
echo "Installing $NEW_VERSION (previous: ${OLD_VERSION:-none}, source: $SOURCE)..."
stop_service
STOPPED=1
# Recheck after the writer has stopped; a restart/config change during download
# must not be hidden by an earlier successful preflight.
UI_URL="$("$DIST_BIN" check --config "$CONFIG_PATH")"
REPLACED=1
for PART in "${PARTS[@]}"; do
  rm -rf "${APP_DIR:?}/$PART"
  mv "$TX/next/$PART" "$APP_DIR/$PART"
done
cp "$TX/service.new" "$SERVICE_PATH.install-new"
chmod 600 "$SERVICE_PATH.install-new"
mv "$SERVICE_PATH.install-new" "$SERVICE_PATH"
if [[ -n "$BOOTSTRAP_LOG" ]]; then : > "$BOOTSTRAP_LOG"; chmod 600 "$BOOTSTRAP_LOG"; fi
if [[ "$PLATFORM" == linux ]]; then systemctl --user daemon-reload; fi
start_service
"$DIST_BIN" check --config "$CONFIG_PATH" --ready --timeout 30s >/dev/null
printf '%s\n' "$SOURCE" > "$APP_DIR/install-source.new"
mv "$APP_DIR/install-source.new" "$APP_DIR/install-source"
STOPPED=0
printf '\nInstallation successful: CC AutoMux %s\nConfig: %s\nWeb UI: %s\n' "$NEW_VERSION" "$CONFIG_PATH" "$UI_URL"
if (( CONFIG_EXISTED )); then echo 'Existing configuration kept unchanged; use its management key to sign in.'; fi
echo 'Login autostart is enabled. Maintenance commands:'
for COMMAND in status stop start uninstall; do printf '  %q\n' "$APP_DIR/scripts/$COMMAND.sh"; done
echo 'Startup diagnostics:'
describe_fallback
