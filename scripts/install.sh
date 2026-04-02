#!/usr/bin/env sh
set -eu

REPO_OWNER="Borgels"
REPO_NAME="clawcontrol-agent"
BINARY_NAME="clawcontrol-agent"
SERVICE_NAME="clawcontrol-agent"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/clawcontrol"
CONFIG_PATH="${CONFIG_DIR}/agent.json"
SYSTEMD_UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
INTERVAL_MS="${CLAWCONTROL_AGENT_INTERVAL_MS:-30000}"
LOG_LEVEL="${CLAWCONTROL_AGENT_LOG_LEVEL:-info}"
INSECURE="${CLAWCONTROL_AGENT_INSECURE:-false}"
VERSION="${CLAWCONTROL_AGENT_VERSION:-latest}"

TOKEN=""
MACHINE_ID=""
SERVER_URL="${CLAWCONTROL_SERVER_URL:-}"
SUDO=""
TMP_DIR=""
BIN_TMP=""
SHA_TMP=""
SHA_EXPECTED=""

usage() {
  cat <<EOF
Usage: install.sh --token <token> --machine <machine-id> --server <server-url> [--version <tag|latest>]

Environment overrides:
  CLAWCONTROL_AGENT_VERSION      Release tag (default: latest)
  CLAWCONTROL_AGENT_INTERVAL_MS  Poll interval in milliseconds (default: 30000)
  CLAWCONTROL_AGENT_LOG_LEVEL    Log level (default: info)
  CLAWCONTROL_AGENT_INSECURE     true|false (default: false)
EOF
}

log() {
  printf '[clawcontrol-agent installer] %s\n' "$*"
}

fatal() {
  printf '[clawcontrol-agent installer] ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}

trap cleanup EXIT INT TERM

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fatal "Required command not found: $1"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --token)
      [ "$#" -ge 2 ] || fatal "Missing value for --token"
      TOKEN="$2"
      shift 2
      ;;
    --machine)
      [ "$#" -ge 2 ] || fatal "Missing value for --machine"
      MACHINE_ID="$2"
      shift 2
      ;;
    --server)
      [ "$#" -ge 2 ] || fatal "Missing value for --server"
      SERVER_URL="$2"
      shift 2
      ;;
    --version)
      [ "$#" -ge 2 ] || fatal "Missing value for --version"
      VERSION="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fatal "Unknown argument: $1"
      ;;
  esac
done

[ -n "$TOKEN" ] || { usage; fatal "Missing --token"; }
[ -n "$MACHINE_ID" ] || { usage; fatal "Missing --machine"; }
[ -n "$SERVER_URL" ] || { usage; fatal "Missing --server"; }

case "$SERVER_URL" in
  https://*|http://*)
    ;;
  *)
    fatal "Server URL must include scheme (https://... or http://...)"
    ;;
esac

if [ "$(id -u)" -ne 0 ]; then
  require_cmd sudo
  SUDO="sudo"
fi

require_cmd uname
require_cmd curl
require_cmd install
require_cmd mktemp
require_cmd chmod
require_cmd awk

if command -v sha256sum >/dev/null 2>&1; then
  SHA256_CMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA256_CMD="shasum -a 256"
elif command -v openssl >/dev/null 2>&1; then
  SHA256_CMD="openssl dgst -sha256"
else
  fatal "Need sha256sum, shasum, or openssl to verify release artifact checksums"
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
[ "$OS" = "linux" ] || fatal "This installer currently supports Linux only"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    fatal "Unsupported architecture: $ARCH"
    ;;
esac

ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}"
if [ "$VERSION" = "latest" ]; then
  RELEASE_BASE="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/latest/download"
else
  RELEASE_BASE="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION}"
fi

ARTIFACT_URL="${RELEASE_BASE}/${ASSET_NAME}"
CHECKSUM_URL="${RELEASE_BASE}/${ASSET_NAME}.sha256"

TMP_DIR="$(mktemp -d)"
BIN_TMP="${TMP_DIR}/${ASSET_NAME}"
SHA_TMP="${TMP_DIR}/${ASSET_NAME}.sha256"

log "Downloading ${ARTIFACT_URL}"
curl --fail --show-error --location --retry 5 --retry-delay 1 --retry-connrefused \
  --output "$BIN_TMP" "$ARTIFACT_URL"

log "Downloading checksum ${CHECKSUM_URL}"
curl --fail --show-error --location --retry 5 --retry-delay 1 --retry-connrefused \
  --output "$SHA_TMP" "$CHECKSUM_URL"

SHA_EXPECTED="$(awk '{print $1}' "$SHA_TMP" | head -n 1)"
[ -n "$SHA_EXPECTED" ] || fatal "Checksum file is empty or invalid"

SHA_ACTUAL="$($SHA256_CMD "$BIN_TMP" | awk '{print $1}')"
[ "$SHA_EXPECTED" = "$SHA_ACTUAL" ] || fatal "Checksum mismatch for downloaded binary"

log "Installing binary to ${INSTALL_DIR}/${BINARY_NAME}"
$SUDO install -d -m 0755 "$INSTALL_DIR"
$SUDO install -m 0755 "$BIN_TMP" "${INSTALL_DIR}/${BINARY_NAME}"

log "Writing config to ${CONFIG_PATH}"
$SUDO install -d -m 0755 "$CONFIG_DIR"
$SUDO sh -c "cat > \"$CONFIG_PATH\" <<EOF
{
  \"serverUrl\": \"${SERVER_URL}\",
  \"token\": \"${TOKEN}\",
  \"machineId\": \"${MACHINE_ID}\",
  \"intervalMs\": ${INTERVAL_MS},
  \"insecure\": ${INSECURE},
  \"logLevel\": \"${LOG_LEVEL}\"
}
EOF"
$SUDO chmod 600 "$CONFIG_PATH"

if command -v systemctl >/dev/null 2>&1; then
  log "Installing systemd unit ${SYSTEMD_UNIT_PATH}"
  $SUDO sh -c "cat > \"$SYSTEMD_UNIT_PATH\" <<EOF
[Unit]
Description=ClawControl Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${BINARY_NAME} --config ${CONFIG_PATH}
Restart=always
RestartSec=5
User=root
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=${CONFIG_DIR}
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF"

  $SUDO systemctl daemon-reload
  $SUDO systemctl enable "$SERVICE_NAME" >/dev/null
  $SUDO systemctl restart "$SERVICE_NAME"
  $SUDO systemctl --no-pager --full status "$SERVICE_NAME" | sed -n '1,8p'
  log "Install complete. Service ${SERVICE_NAME} is enabled and restarted."
else
  log "systemd not detected. Start manually:"
  log "${INSTALL_DIR}/${BINARY_NAME} --config ${CONFIG_PATH}"
fi
