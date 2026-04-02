# clawcontrol-agent

Lightweight machine agent for ClawControl.

## What it does

- Performs periodic authenticated check-ins to `POST /api/agent/checkin`
- Reports discovered machine metadata (hostname, addresses, hardware summary)
- Pulls pending commands from ClawControl
- Executes allowlisted commands (`install_runtime`, `pull_model`, `remove_model`, `restart_runtime`, `health_check`, `update_agent`)
- Acknowledges command result to `POST /api/agent/ack`

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
