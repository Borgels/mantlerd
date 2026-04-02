#!/usr/bin/env sh
set -eu

TOKEN=""
MACHINE_ID=""
SERVER_URL="${CLAWCONTROL_SERVER_URL:-}"
VERSION="${CLAWCONTROL_AGENT_VERSION:-latest}"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/clawcontrol"
CONFIG_PATH="${CONFIG_DIR}/agent.json"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --token)
      TOKEN="$2"
      shift 2
      ;;
    --machine)
      MACHINE_ID="$2"
      shift 2
      ;;
    --server)
      SERVER_URL="$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

if [ -z "$TOKEN" ] || [ -z "$MACHINE_ID" ] || [ -z "$SERVER_URL" ]; then
  echo "Usage: install.sh --token <token> --machine <machine-id> --server <server-url>" >&2
  exit 1
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

if [ "$VERSION" = "latest" ]; then
  ARTIFACT_URL="https://github.com/Borgels/clawcontrol-agent/releases/latest/download/clawcontrol-agent-${OS}-${ARCH}"
else
  ARTIFACT_URL="https://github.com/Borgels/clawcontrol-agent/releases/download/${VERSION}/clawcontrol-agent-${OS}-${ARCH}"
fi

echo "Downloading ${ARTIFACT_URL}"
curl -fsSL "$ARTIFACT_URL" -o /tmp/clawcontrol-agent

install -m 0755 /tmp/clawcontrol-agent "${INSTALL_DIR}/clawcontrol-agent"
rm -f /tmp/clawcontrol-agent

mkdir -p "$CONFIG_DIR"
cat > "$CONFIG_PATH" <<EOF
{
  "serverUrl": "${SERVER_URL}",
  "token": "${TOKEN}",
  "machineId": "${MACHINE_ID}",
  "intervalMs": 30000,
  "insecure": false,
  "logLevel": "info"
}
EOF
chmod 600 "$CONFIG_PATH"

if command -v systemctl >/dev/null 2>&1; then
  cat > /etc/systemd/system/clawcontrol-agent.service <<EOF
[Unit]
Description=ClawControl Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/clawcontrol-agent --config /etc/clawcontrol/agent.json
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now clawcontrol-agent
  echo "Installed and started clawcontrol-agent.service"
else
  echo "systemd not detected. Run manually:"
  echo "/usr/local/bin/clawcontrol-agent --config /etc/clawcontrol/agent.json"
fi
