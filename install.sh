#!/bin/sh
set -eu

SERVICE_NAME="${SERVICE_NAME:-website-proxy}"
INSTALL_DIR="${INSTALL_DIR:-/opt/website-proxy}"
REPO_OWNER="${REPO_OWNER:-Assute}"
REPO_NAME="${REPO_NAME:-website-proxy}"
REPO_BRANCH="${REPO_BRANCH:-main}"
OVERWRITE_CONFIG="${OVERWRITE_CONFIG:-0}"

RAW_BASE="${RAW_BASE:-https://raw.githubusercontent.com/$REPO_OWNER/$REPO_NAME/$REPO_BRANCH}"
BIN_URL="$RAW_BASE/website-proxy"
INIT_URL="$RAW_BASE/website-proxy.initd"
CONFIG_URL="$RAW_BASE/config.json"

BIN_PATH="$INSTALL_DIR/website-proxy"
INIT_PATH="/etc/init.d/$SERVICE_NAME"
CONFIG_PATH="$INSTALL_DIR/config.json"

log() {
  printf '%s\n' "$*"
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    log "This installer must run as root."
    exit 1
  fi
}

download() {
  url="$1"
  out="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
    return 0
  fi

  if command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
    return 0
  fi

  log "curl/wget not found. Please install one of them first."
  exit 1
}

require_cmd() {
  cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    log "Required command not found: $cmd"
    exit 1
  fi
}

install_files() {
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT INT TERM

  mkdir -p "$INSTALL_DIR"

  log "Downloading binary..."
  download "$BIN_URL" "$tmp_dir/website-proxy"

  log "Downloading OpenRC script..."
  download "$INIT_URL" "$tmp_dir/website-proxy.initd"

  if [ -f "$CONFIG_PATH" ] && [ "$OVERWRITE_CONFIG" != "1" ]; then
    log "Keeping existing config: $CONFIG_PATH"
  else
    log "Downloading config.json..."
    download "$CONFIG_URL" "$tmp_dir/config.json"
    mv "$tmp_dir/config.json" "$CONFIG_PATH"
  fi

  mv "$tmp_dir/website-proxy" "$BIN_PATH"
  mv "$tmp_dir/website-proxy.initd" "$INIT_PATH"

  chmod +x "$BIN_PATH"
  chmod +x "$INIT_PATH"

  trap - EXIT INT TERM
  rm -rf "$tmp_dir"
}

restart_service() {
  if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
    rc-service "$SERVICE_NAME" stop || true
  fi

  pkill -f "$BIN_PATH" >/dev/null 2>&1 || true
  rm -f "/run/$SERVICE_NAME.pid"

  rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 || true
  rc-service "$SERVICE_NAME" start
}

show_result() {
  port_text="$(grep -E '"port"\s*:' "$CONFIG_PATH" 2>/dev/null | head -n1 | sed 's/[^0-9]//g' || true)"
  if [ -z "$port_text" ]; then
    port_text="16800"
  fi

  log ""
  log "Install complete."
  log "Service: $SERVICE_NAME"
  log "Binary:  $BIN_PATH"
  log "Config:  $CONFIG_PATH"
  log "Status:"
  rc-service "$SERVICE_NAME" status || true
  log ""
  log "Entry example: http://your-server:$port_text/go"
}

main() {
  require_root
  require_cmd rc-service
  require_cmd rc-update
  install_files
  restart_service
  show_result
}

main "$@"
