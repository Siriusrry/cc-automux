#!/usr/bin/env bash
set -euo pipefail

# Keep the streamed entry point in one function: incomplete downloads cannot
# execute a partially received installation transaction.
install_main() {
  local source_file="${BASH_SOURCE[0]:-}" script_dir platform arch tag url work asset expected actual member
  if [[ $# -eq 1 && "$1" == "--local" ]]; then
    [[ -n "$source_file" && -f "$source_file" ]] || { echo "--local requires a local project checkout." >&2; return 1; }
    script_dir="$(cd "$(dirname "$source_file")" && pwd)"
    bash "$script_dir/_install.sh" local
    return
  fi
  if [[ $# -gt 0 ]]; then
    echo "Usage: install.sh [--local]" >&2
    return 2
  fi
  case "$(uname -s)" in
    Darwin) platform=darwin ;;
    Linux) platform=linux ;;
    *) echo "Unsupported platform: macOS and Linux are supported." >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "Unsupported CPU architecture: $(uname -m)." >&2; return 1 ;;
  esac
  command -v curl >/dev/null || { echo "curl is required." >&2; return 1; }
  command -v tar >/dev/null || { echo "tar is required." >&2; return 1; }
  if ! command -v sha256sum >/dev/null && ! command -v shasum >/dev/null; then
    echo "sha256sum or shasum is required." >&2; return 1
  fi
  umask 077
  work="$(mktemp -d "${TMPDIR:-/tmp}/cc-automux-download.XXXXXX")"
  # A subshell owns cleanup, including failure and signal paths.
  (
    trap 'rm -rf "$work"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    url="$(curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 -o /dev/null -w '%{url_effective}' https://github.com/Siriusrry/cc-automux/releases/latest)" || { echo "Cannot resolve the latest public stable Release." >&2; exit 1; }
    tag="${url#https://github.com/Siriusrry/cc-automux/releases/tag/}"
    [[ "$url" != "$tag" && "$tag" =~ ^v[1-9][0-9]*\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo "No supported stable v1-or-newer Release is available." >&2; exit 1; }
    asset="cc-automux_${tag}_${platform}_${arch}.tar.gz"
    url="https://github.com/Siriusrry/cc-automux/releases/download/$tag"
    echo "Downloading CC AutoMux $tag ($platform/$arch)..."
    curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 180 "$url/$asset" -o "$work/$asset" || { echo "Release package download failed: $asset" >&2; exit 1; }
    curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 "$url/SHA256SUMS" -o "$work/SHA256SUMS" || { echo "Release checksum download failed." >&2; exit 1; }
    expected="$(awk -v name="$asset" '$2 == name {print $1}' "$work/SHA256SUMS")"
    [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || { echo "Missing or ambiguous package checksum." >&2; exit 1; }
    if command -v sha256sum >/dev/null; then
      actual="$(sha256sum "$work/$asset")"
    else
      actual="$(shasum -a 256 "$work/$asset")"
    fi
    [[ "${actual%% *}" == "$expected" ]] || { echo "Package SHA-256 mismatch." >&2; exit 1; }
    tar -tzf "$work/$asset" > "$work/members"
    while IFS= read -r member; do
      case "/$member/" in
        //*|*/../*|*/./*) echo "Unsafe release package path." >&2; exit 1 ;;
      esac
    done < "$work/members"
    tar -tvzf "$work/$asset" > "$work/types"
    if awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" {bad=1} END {exit !bad}' "$work/types"; then
      echo "Release package contains links or special files." >&2; exit 1
    fi
    mkdir "$work/package"
    tar -xzf "$work/$asset" -C "$work/package"
    [[ -x "$work/package/dist/cc-automux" && -f "$work/package/scripts/_install.sh" ]] || { echo "Release package is incomplete." >&2; exit 1; }
    [[ "$("$work/package/dist/cc-automux" --platform)" == "$platform/$arch" ]] || { echo "Package platform mismatch." >&2; exit 1; }
    [[ "$("$work/package/dist/cc-automux" --version)" == "CC AutoMux $tag" ]] || { echo "Package version mismatch." >&2; exit 1; }
    bash "$work/package/scripts/_install.sh" public "$tag"
  )
}

install_main "$@"
