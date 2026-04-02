# clawcontrol-agent

Lightweight machine agent for ClawControl.

## What it does

- Performs periodic authenticated check-ins to `POST /api/agent/checkin`
- Reports discovered machine metadata (hostname, addresses, hardware summary)
- Pulls pending commands from ClawControl
- Executes allowlisted commands (`install_runtime`, `pull_model`, `remove_model`, `restart_runtime`, `health_check`, `update_agent`)
- Acknowledges command result to `POST /api/agent/ack`

`update_agent` command notes:

- Starts an in-place agent update using the official installer script.
- Accepts optional `params.version` (`latest` by default).
- Agent reports the installed version via `agentVersion` on subsequent check-ins.

`health_check` command notes:

- When `params.scope` is `"model_benchmark"`, the agent runs a real Ollama benchmark via `/api/generate`.
- Ack `details` includes benchmark JSON under `benchmark`:
  - `ttftMs`
  - `outputTokensPerSec`
  - `totalLatencyMs`
  - `promptTokensPerSec`
  - `p95TtftMsAtSmallConcurrency`

## Quick start

```bash
go build -o clawcontrol-agent ./cmd/clawcontrol-agent
./clawcontrol-agent \
  --server https://control.example.com \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID
```

Config is persisted by default at:

- `/etc/clawcontrol/agent.json` when run as root
- `~/.clawcontrol/agent.json` otherwise

## Security defaults

- Requires HTTPS unless `--insecure` is explicitly set
- Sends bearer token on every request
- Stores config with `0600` permissions
- Only executes typed, allowlisted commands

## Build targets

```bash
make build
make release
```

## Linux installer (recommended)

```bash
curl -fsSL https://install.clawcontrol.dev | sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server https://control.example.com
```

Installer behavior:

- Linux-only (systemd target)
- Requires root privileges (or passwordless sudo)
- Downloads release binary from GitHub Releases
- Verifies SHA-256 using `<asset>.sha256`
- Installs binary to `/usr/local/bin/clawcontrol-agent`
- Writes config to `/etc/clawcontrol/agent.json` (`0600`)
- Creates/updates and restarts `clawcontrol-agent.service`
- Writes `ollama.service` systemd override with `OLLAMA_HOST=0.0.0.0:11434` (configurable)
- Runs post-install service health checks and prints diagnostics on failure
- Auto-enables insecure mode for `http://` server URLs (or use `--insecure`)
- Agent runtime also auto-enables insecure mode for `http://` server URLs as a safety fallback

Required release assets:

- `clawcontrol-agent-linux-amd64`
- `clawcontrol-agent-linux-amd64.sha256`
- `clawcontrol-agent-linux-arm64`
- `clawcontrol-agent-linux-arm64.sha256`
