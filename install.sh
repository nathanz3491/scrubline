#!/bin/sh
# Install scrubline.
#
#   curl -fsSL https://raw.githubusercontent.com/nathanz3491/scrubline/main/install.sh | sh
#
# Environment:
#   SCRUBLINE_VERSION      version to install, e.g. v0.1.0 (default: latest)
#   SCRUBLINE_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   SCRUBLINE_BASE_URL     where to fetch archives from, for testing a local build
set -eu

REPO="nathanz3491/scrubline"
BIN="scrubline"
INSTALL_DIR="${SCRUBLINE_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "this installer needs $1 on PATH"; }

detect_platform() {
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$os" in
    linux) os=linux ;;
    darwin) os=darwin ;;
    msys*|mingw*|cygwin*) os=windows ;;
    *) die "unsupported operating system: $os" ;;
  esac
  case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
  esac
  if [ "$os" = windows ] && [ "$arch" = arm64 ]; then
    die "windows/arm64 is not published"
  fi
  PLATFORM="${os}_${arch}"
  EXT=tar.gz
  if [ "$os" = windows ]; then
    EXT=zip
  fi
}

resolve_version() {
  if [ -n "${SCRUBLINE_VERSION:-}" ]; then
    VERSION="$SCRUBLINE_VERSION"
    return
  fi
  # Follow the /releases/latest redirect rather than using the API, so the
  # installer works without a token and without jq.
  location=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") \
    || die "could not reach GitHub to find the latest release"
  VERSION=${location##*/}
  [ -n "$VERSION" ] || die "could not determine the latest version"
}

# POSIX sh has no function-local variables, so anything assigned here is global.
# The leading underscores keep this from clobbering the caller's state.
verify_checksum() {
  _vc_file=$1
  _vc_sums=$2
  _vc_name=$(basename "$_vc_file")
  _vc_want=$(grep " $_vc_name\$" "$_vc_sums" | awk '{print $1}')
  [ -n "$_vc_want" ] || die "no checksum published for $_vc_name"
  if command -v sha256sum >/dev/null 2>&1; then
    _vc_got=$(sha256sum "$_vc_file" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    _vc_got=$(shasum -a 256 "$_vc_file" | awk '{print $1}')
  else
    die "this installer needs sha256sum or shasum to verify the download"
  fi
  [ "$_vc_want" = "$_vc_got" ] || die "checksum mismatch for $_vc_name: expected $_vc_want, got $_vc_got"
  say "  checksum ok"
}

main() {
  need curl
  need tar
  detect_platform

  if [ -n "${SCRUBLINE_BASE_URL:-}" ]; then
    BASE="$SCRUBLINE_BASE_URL"
    VERSION="${SCRUBLINE_VERSION:-local}"
  else
    resolve_version
    BASE="https://github.com/$REPO/releases/download/$VERSION"
  fi

  archive="${BIN}_${PLATFORM}.${EXT}"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "scrubline $VERSION for $PLATFORM"
  curl -fsSL "$BASE/$archive" -o "$tmp/$archive" || die "could not download $BASE/$archive"
  curl -fsSL "$BASE/checksums.txt" -o "$tmp/checksums.txt" || die "could not download checksums.txt"
  verify_checksum "$tmp/$archive" "$tmp/checksums.txt"

  case "$archive" in
    *.zip) need unzip; unzip -q "$tmp/$archive" -d "$tmp" ;;
    *) tar -xzf "$tmp/$archive" -C "$tmp" ;;
  esac
  [ -f "$tmp/$BIN" ] || die "archive did not contain a $BIN binary"

  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$tmp/$BIN" "$INSTALL_DIR/$BIN" 2>/dev/null \
    || { cp "$tmp/$BIN" "$INSTALL_DIR/$BIN" && chmod 0755 "$INSTALL_DIR/$BIN"; } \
    || die "could not write to $INSTALL_DIR (set SCRUBLINE_INSTALL_DIR to somewhere writable)"

  say "  installed $INSTALL_DIR/$BIN"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) say "run: $BIN --help" ;;
    *) say ""; say "$INSTALL_DIR is not on your PATH. Add it:"; say "  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

main "$@"
