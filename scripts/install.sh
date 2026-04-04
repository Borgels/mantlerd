#!/usr/bin/env sh
set -eu

REPO_OWNER="Borgels"
REPO_NAME="clawcontrol-agent"
BINARY_NAME="clawcontrol-agent"
CLI_NAME="clawcontrol"
SERVICE_NAME="clawcontrol-agent"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/clawcontrol"
CONFIG_PATH="${CONFIG_DIR}/agent.json"
SYSTEMD_UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
OLLAMA_OVERRIDE_DIR="/etc/systemd/system/ollama.service.d"
OLLAMA_OVERRIDE_PATH="${OLLAMA_OVERRIDE_DIR}/override.conf"
VLLM_UNIT_PATH="/etc/systemd/system/vllm.service"
VLLM_CONFIG_PATH="${CONFIG_DIR}/vllm.json"
VLLM_ENV_PATH="${CONFIG_DIR}/vllm.env"
VLLM_VENV_PATH="/opt/clawcontrol/vllm-venv"
VLLM_PYTHON="${VLLM_VENV_PATH}/bin/python3"
INTERVAL_MS="${CLAWCONTROL_AGENT_INTERVAL_MS:-30000}"
LOG_LEVEL="${CLAWCONTROL_AGENT_LOG_LEVEL:-info}"
INSECURE="${CLAWCONTROL_AGENT_INSECURE:-false}"
VERSION="${CLAWCONTROL_AGENT_VERSION:-latest}"
OLLAMA_CONFIGURE_REMOTE="${CLAWCONTROL_OLLAMA_CONFIGURE_REMOTE:-true}"
OLLAMA_HOST="${CLAWCONTROL_OLLAMA_HOST:-0.0.0.0:11434}"
VLLM_CONFIGURE="${CLAWCONTROL_VLLM_CONFIGURE:-true}"
VLLM_PREINSTALL="${CLAWCONTROL_VLLM_PREINSTALL:-true}"
VLLM_RUNTIME_MODE="${CLAWCONTROL_VLLM_RUNTIME_MODE:-auto}"
VLLM_PORT="${CLAWCONTROL_VLLM_PORT:-8000}"
VLLM_GPU_MEMORY_UTILIZATION="${CLAWCONTROL_VLLM_GPU_MEMORY_UTILIZATION:-0.9}"
VLLM_TRUST_REMOTE_CODE="${CLAWCONTROL_VLLM_TRUST_REMOTE_CODE:-false}"
VLLM_EXTRA_ARGS="${CLAWCONTROL_VLLM_EXTRA_ARGS:-}"
VLLM_HF_TOKEN="${CLAWCONTROL_HF_TOKEN:-${HF_TOKEN:-}}"
VLLM_HF_HUB_TOKEN="${CLAWCONTROL_HUGGING_FACE_HUB_TOKEN:-${HUGGING_FACE_HUB_TOKEN:-}}"
SELF_UPDATE_MODE="${CLAWCONTROL_AGENT_SELF_UPDATE:-false}"

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
Usage: install.sh --token <token> --machine <machine-id> --server <server-url> [--version <tag|latest>] [--insecure]

Environment overrides:
  CLAWCONTROL_AGENT_VERSION      Release tag (default: latest)
  CLAWCONTROL_AGENT_INTERVAL_MS  Poll interval in milliseconds (default: 30000)
  CLAWCONTROL_AGENT_LOG_LEVEL    Log level (default: info)
  CLAWCONTROL_AGENT_INSECURE     true|false (default: false)
  CLAWCONTROL_OLLAMA_CONFIGURE_REMOTE  true|false (default: true)
  CLAWCONTROL_OLLAMA_HOST        Ollama bind host:port (default: 0.0.0.0:11434)
  CLAWCONTROL_VLLM_CONFIGURE     true|false (default: true)
  CLAWCONTROL_VLLM_PREINSTALL    true|false (default: true)
  CLAWCONTROL_VLLM_RUNTIME_MODE  auto|container|native (default: auto)
  CLAWCONTROL_VLLM_PORT          vLLM OpenAI API port (default: 8000)
  CLAWCONTROL_VLLM_GPU_MEMORY_UTILIZATION GPU memory utilization fraction (default: 0.9)
  CLAWCONTROL_VLLM_TRUST_REMOTE_CODE true|false (default: false)
  CLAWCONTROL_VLLM_EXTRA_ARGS    extra CLI args appended to vLLM serve
  CLAWCONTROL_HF_TOKEN           optional Hugging Face token persisted for vLLM model pulls
  CLAWCONTROL_HUGGING_FACE_HUB_TOKEN optional Hugging Face Hub token persisted for vLLM model pulls
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

wait_for_service() {
  service="$1"
  retries="${2:-12}"
  i=0
  while [ "$i" -lt "$retries" ]; do
    state="$($SUDO systemctl is-active "$service" 2>/dev/null || true)"
    if [ "$state" = "active" ]; then
      return 0
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
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
    --insecure)
      INSECURE="true"
      shift
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

case "$INSECURE" in
  true|false)
    ;;
  *)
    fatal "CLAWCONTROL_AGENT_INSECURE must be true or false"
    ;;
esac

case "$VLLM_CONFIGURE" in
  true|false)
    ;;
  *)
    fatal "CLAWCONTROL_VLLM_CONFIGURE must be true or false"
    ;;
esac

case "$VLLM_PREINSTALL" in
  true|false)
    ;;
  *)
    fatal "CLAWCONTROL_VLLM_PREINSTALL must be true or false"
    ;;
esac

case "$VLLM_RUNTIME_MODE" in
  auto|native|container)
    ;;
  *)
    fatal "CLAWCONTROL_VLLM_RUNTIME_MODE must be auto, native, or container"
    ;;
esac

case "$VLLM_TRUST_REMOTE_CODE" in
  true|false)
    ;;
  *)
    fatal "CLAWCONTROL_VLLM_TRUST_REMOTE_CODE must be true or false"
    ;;
esac

case "$SELF_UPDATE_MODE" in
  true|false)
    ;;
  *)
    fatal "CLAWCONTROL_AGENT_SELF_UPDATE must be true or false"
    ;;
esac

case "$SERVER_URL" in
  http://*)
    if [ "$INSECURE" != "true" ]; then
      INSECURE="true"
    fi
    log "Detected non-HTTPS server URL. Insecure mode is enabled for agent transport."
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
log "Linking CLI command ${CLI_NAME} -> ${BINARY_NAME}"
$SUDO ln -sf "${INSTALL_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${CLI_NAME}"

HEALTH_URL="${SERVER_URL%/}/api/health"
log "Preflight: checking server reachability at ${HEALTH_URL}"
if curl --silent --show-error --location --max-time 8 --output /dev/null "$HEALTH_URL"; then
  log "Preflight: server is reachable."
else
  log "Preflight warning: server not reachable right now. Agent will retry in background."
fi

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
$SUDO install -d -m 0755 "${CONFIG_DIR}/.config"
$SUDO install -d -m 0755 "${CONFIG_DIR}/.config/clawcontrol-agent"

if command -v systemctl >/dev/null 2>&1; then
  log "Installing systemd unit ${SYSTEMD_UNIT_PATH}"
  $SUDO sh -c "cat > \"$SYSTEMD_UNIT_PATH\" <<EOF
[Unit]
Description=ClawControl Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${BINARY_NAME} start --config ${CONFIG_PATH}
Restart=always
RestartSec=5
User=root
Environment=HOME=${CONFIG_DIR}
Environment=XDG_CONFIG_HOME=${CONFIG_DIR}/.config
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ReadWritePaths=${CONFIG_DIR}
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF"

  $SUDO systemctl daemon-reload
  $SUDO systemctl enable "$SERVICE_NAME" >/dev/null
  if [ "$SELF_UPDATE_MODE" = "true" ]; then
    log "Self-update mode detected. Triggering non-blocking service restart."
    $SUDO systemctl --no-block restart "$SERVICE_NAME"
    log "Self-update restart triggered; installer exiting without in-process health wait."
    exit 0
  fi

  $SUDO systemctl restart "$SERVICE_NAME"

  if [ "$OLLAMA_CONFIGURE_REMOTE" = "true" ]; then
    log "Configuring ollama systemd override (OLLAMA_HOST=${OLLAMA_HOST})"
    $SUDO install -d -m 0755 "$OLLAMA_OVERRIDE_DIR"
    $SUDO sh -c "cat > \"$OLLAMA_OVERRIDE_PATH\" <<EOF
[Service]
Environment=\"OLLAMA_HOST=${OLLAMA_HOST}\"
EOF"
    $SUDO chmod 0644 "$OLLAMA_OVERRIDE_PATH"
    $SUDO systemctl daemon-reload
    if $SUDO systemctl status ollama >/dev/null 2>&1; then
      $SUDO systemctl restart ollama
      log "Restarted ollama service with remote bind override."
    else
      log "ollama service not detected yet. Override was written and will apply once installed."
    fi
  else
    log "Skipping ollama remote bind override (CLAWCONTROL_OLLAMA_CONFIGURE_REMOTE=${OLLAMA_CONFIGURE_REMOTE})."
  fi

  if [ "$VLLM_CONFIGURE" = "true" ]; then
    log "Preparing vLLM service template and config"
    if command -v nvidia-smi >/dev/null 2>&1; then
      log "nvidia-smi detected; GPU runtime appears available for vLLM."
    else
      log "vLLM preflight warning: nvidia-smi not found. vLLM may fail without NVIDIA drivers/CUDA."
    fi

    $SUDO install -d -m 0755 "$CONFIG_DIR"
    if [ ! -f "$VLLM_CONFIG_PATH" ]; then
      $SUDO sh -c "cat > \"$VLLM_CONFIG_PATH\" <<EOF
{
  \"model\": \"\",
  \"port\": ${VLLM_PORT}
}
EOF"
      $SUDO chmod 600 "$VLLM_CONFIG_PATH"
    fi

    $SUDO sh -c "cat > \"$VLLM_ENV_PATH\" <<EOF
VLLM_MODEL=
VLLM_PORT=${VLLM_PORT}
VLLM_GPU_MEMORY_UTILIZATION=${VLLM_GPU_MEMORY_UTILIZATION}
VLLM_LD_LIBRARY_PATH=
VLLM_TRUST_REMOTE_CODE=${VLLM_TRUST_REMOTE_CODE}
VLLM_EXTRA_ARGS="${VLLM_EXTRA_ARGS}"
VLLM_RUNTIME_MODE=${VLLM_RUNTIME_MODE}
EOF"
    if [ -n "$VLLM_HF_TOKEN" ]; then
      VLLM_ESCAPED_HF_TOKEN="$(printf '%s' "$VLLM_HF_TOKEN" | sed 's/\\/\\\\/g; s/"/\\"/g')"
      $SUDO sh -c "printf '%s\n' \"HF_TOKEN=\\\"${VLLM_ESCAPED_HF_TOKEN}\\\"\" >> \"$VLLM_ENV_PATH\""
    fi
    if [ -n "$VLLM_HF_HUB_TOKEN" ]; then
      VLLM_ESCAPED_HF_HUB_TOKEN="$(printf '%s' "$VLLM_HF_HUB_TOKEN" | sed 's/\\/\\\\/g; s/"/\\"/g')"
      $SUDO sh -c "printf '%s\n' \"HUGGING_FACE_HUB_TOKEN=\\\"${VLLM_ESCAPED_HF_HUB_TOKEN}\\\"\" >> \"$VLLM_ENV_PATH\""
    fi
    $SUDO chmod 600 "$VLLM_ENV_PATH"

    $SUDO sh -c "cat > \"$VLLM_UNIT_PATH\" <<EOF
[Unit]
Description=vLLM OpenAI API Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-${VLLM_ENV_PATH}
ExecStart=/bin/sh -c 'LD_LIBRARY_PATH=\"\${VLLM_LD_LIBRARY_PATH:-}\${LD_LIBRARY_PATH:+:\${LD_LIBRARY_PATH}}\"; EXTRA_ARGS=\"\${VLLM_EXTRA_ARGS:-}\"; if [ \"\${VLLM_TRUST_REMOTE_CODE:-false}\" = \"true\" ]; then EXTRA_ARGS=\"\${EXTRA_ARGS} --trust-remote-code\"; fi; exec ${VLLM_PYTHON} -m vllm.entrypoints.openai.api_server --model \"\${VLLM_MODEL}\" --host 0.0.0.0 --port \"\${VLLM_PORT:-8000}\" --gpu-memory-utilization \"\${VLLM_GPU_MEMORY_UTILIZATION:-0.9}\" \${EXTRA_ARGS}'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
EOF"
    $SUDO chmod 0644 "$VLLM_UNIT_PATH"
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable vllm >/dev/null || true

    VLLM_EFFECTIVE_MODE="$VLLM_RUNTIME_MODE"
    if [ "$VLLM_EFFECTIVE_MODE" = "auto" ]; then
      if command -v docker >/dev/null 2>&1; then
        VLLM_EFFECTIVE_MODE="container"
      else
        VLLM_EFFECTIVE_MODE="native"
      fi
    fi

    if [ "$VLLM_PREINSTALL" = "true" ] && [ "$VLLM_EFFECTIVE_MODE" != "container" ]; then
      log "Preinstalling vLLM in dedicated virtualenv (${VLLM_VENV_PATH})"
      if ! command -v python3 >/dev/null 2>&1; then
        log "vLLM preinstall warning: python3 not found. Runtime install command will retry later."
      else
        $SUDO install -d -m 0755 "$VLLM_VENV_PATH"
        if ! $SUDO python3 -m venv "$VLLM_VENV_PATH"; then
          log "vLLM preinstall warning: python3 venv creation failed. Trying to install python3-venv."
          if command -v apt-get >/dev/null 2>&1; then
            $SUDO sh -c "DEBIAN_FRONTEND=noninteractive apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y python3-venv python3-pip" || true
            $SUDO python3 -m venv "$VLLM_VENV_PATH" || true
          fi
        fi
        if [ -x "$VLLM_PYTHON" ]; then
          $SUDO "$VLLM_PYTHON" -m pip install --upgrade pip || true
          if ! $SUDO "$VLLM_PYTHON" -m pip install --upgrade vllm; then
            log "vLLM preinstall warning: pip install vllm failed. Agent self-heal/runtime install will retry."
          else
            if ! $SUDO "$VLLM_PYTHON" -c 'import ctypes; ctypes.CDLL("libcudart.so.12")' >/dev/null 2>&1; then
              log "vLLM preinstall: libcudart.so.12 missing; trying nvidia-cuda-runtime-cu12 wheel."
              $SUDO "$VLLM_PYTHON" -m pip install --upgrade nvidia-cuda-runtime-cu12 || true
            fi
            VLLM_LIB_PATHS="$($SUDO "$VLLM_PYTHON" -c 'import glob, os, site; paths=[]; [paths.append(p) for b in site.getsitepackages() for pat in ("nvidia/*/lib","torch/lib") for p in glob.glob(os.path.join(b, pat)) if os.path.isdir(p)]; seen=set(); ordered=[]; [ordered.append(p) for p in paths if not (p in seen or seen.add(p))]; print(":".join(ordered))' 2>/dev/null || true)"
            if [ -n "$VLLM_LIB_PATHS" ]; then
              $SUDO sh -c "awk 'BEGIN{written=0} /^VLLM_LD_LIBRARY_PATH=/{print \"VLLM_LD_LIBRARY_PATH=${VLLM_LIB_PATHS}\"; written=1; next} {print} END{if(!written) print \"VLLM_LD_LIBRARY_PATH=${VLLM_LIB_PATHS}\"}' \"$VLLM_ENV_PATH\" > \"$VLLM_ENV_PATH.tmp\" && mv \"$VLLM_ENV_PATH.tmp\" \"$VLLM_ENV_PATH\""
              $SUDO chmod 600 "$VLLM_ENV_PATH"
            fi
            if [ "${VLLM_TRUST_REMOTE_CODE}" = "true" ]; then
              $SUDO sh -c "awk 'BEGIN{written=0} /^VLLM_TRUST_REMOTE_CODE=/{print \"VLLM_TRUST_REMOTE_CODE=true\"; written=1; next} {print} END{if(!written) print \"VLLM_TRUST_REMOTE_CODE=true\"}' \"$VLLM_ENV_PATH\" > \"$VLLM_ENV_PATH.tmp\" && mv \"$VLLM_ENV_PATH.tmp\" \"$VLLM_ENV_PATH\""
              $SUDO chmod 600 "$VLLM_ENV_PATH"
            fi
            log "vLLM preinstall complete."
          fi
        else
          log "vLLM preinstall warning: virtualenv python missing at ${VLLM_PYTHON}."
        fi
      fi
    else
      log "Skipping vLLM package preinstall (CLAWCONTROL_VLLM_PREINSTALL=${VLLM_PREINSTALL}, mode=${VLLM_EFFECTIVE_MODE})."
    fi

    log "vLLM service template installed. It will be started when a model is configured."
  else
    log "Skipping vLLM template install (CLAWCONTROL_VLLM_CONFIGURE=${VLLM_CONFIGURE})."
  fi

  if wait_for_service "$SERVICE_NAME" 15; then
    log "Service ${SERVICE_NAME} is active."
    $SUDO systemctl --no-pager --full status "$SERVICE_NAME" | sed -n '1,8p'
    log "Install complete. Service ${SERVICE_NAME} is enabled and restarted."
  else
    log "Service ${SERVICE_NAME} failed to become active. Showing diagnostics:"
    $SUDO systemctl --no-pager --full status "$SERVICE_NAME" || true
    if command -v journalctl >/dev/null 2>&1; then
      $SUDO journalctl -u "$SERVICE_NAME" -n 40 --no-pager || true
    fi
    fatal "Agent service is not healthy. Check server URL/reachability and token."
  fi
else
  log "systemd not detected. Start manually:"
  log "${INSTALL_DIR}/${BINARY_NAME} --config ${CONFIG_PATH}"
fi
