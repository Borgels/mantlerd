# clawcontrol-agent

ClawControl machine agent for runtime and model orchestration.

The agent runs on each worker machine, checks in with ClawControl, executes typed commands, and reports runtime/model state back to the control plane.

## Overview

`clawcontrol-agent` does three core things:

- Performs periodic authenticated check-ins to ClawControl (`/api/agent/checkin`)
- Executes allowlisted commands returned by the server
- Acknowledges command results (`/api/agent/ack`)

It also reports machine metadata:

- Hostname and discovered addresses
- Hardware summary (CPU, RAM, GPU)
- Installed/ready runtimes and versions
- Installed models and model status
- Agent version

## Runtime lifecycle model

The agent and runtimes are intentionally separate concerns:

- **Agent updates** update agent code and service behavior.
- **Runtime changes** (install/restart/reconfigure) are done via runtime commands.
- Routine runtime control should not force implicit runtime upgrades.

In practice, the agent controls runtimes but does not treat runtime version drift as an automatic agent-upgrade side effect.

## Supported server commands

The executor currently supports:

- `install_runtime`
- `uninstall_runtime`
- `restart_runtime`
- `pull_model`
- `remove_model`
- `health_check`
- `update_agent`

## Installation (Linux, recommended)

Use the release installer script:

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/clawcontrol-agent/master/scripts/install.sh | \
  sudo sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server https://control.example.com
```

For non-HTTPS ClawControl endpoints:

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/clawcontrol-agent/master/scripts/install.sh | \
  sudo sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server http://control.local:3400 \
  --insecure
```

Pin to a specific version:

```bash
... --version v0.2.9
```

### What the installer sets up

- Installs `/usr/local/bin/clawcontrol-agent`
- Creates CLI alias `/usr/local/bin/clawcontrol` (symlink)
- Writes config file `/etc/clawcontrol/agent.json` (`0600`)
- Installs systemd unit `/etc/systemd/system/clawcontrol-agent.service`
- Starts daemon with:
  - `ExecStart=/usr/local/bin/clawcontrol-agent start --config /etc/clawcontrol/agent.json`
- Enables + restarts the service
- Installs/updates runtime templates:
  - Ollama remote bind override (`OLLAMA_HOST=0.0.0.0:11434` by default)
  - vLLM systemd template and env/config files

## Configuration

Default config path:

- root: `/etc/clawcontrol/agent.json`
- non-root: `~/.clawcontrol/agent.json`

Config schema:

```json
{
  "serverUrl": "https://control.example.com",
  "token": "MACHINE_TOKEN",
  "machineId": "spark01",
  "intervalMs": 30000,
  "insecure": false,
  "logLevel": "info"
}
```

Manage config via CLI:

```bash
clawcontrol config show
clawcontrol config path
clawcontrol config set server https://control.example.com
clawcontrol config set interval 30s
```

## CLI quick reference

Top-level:

```bash
clawcontrol --help
clawcontrol version
clawcontrol update check
clawcontrol update apply --yes
clawcontrol doctor
clawcontrol info
clawcontrol start
clawcontrol checkin
```

Runtime management:

```bash
clawcontrol runtime list
clawcontrol runtime status
clawcontrol runtime install vllm
clawcontrol runtime restart vllm
```

Model management:

```bash
clawcontrol model list
clawcontrol model pull nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4 --runtime vllm
clawcontrol model remove nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4 --runtime vllm
clawcontrol model benchmark llama2:7b --profile standard
```

## vLLM notes

The agent supports both native and containerized vLLM modes.

- Runtime mode defaults to `auto` (container if Docker is available)
- vLLM env/config files:
  - `/etc/clawcontrol/vllm.json`
  - `/etc/clawcontrol/vllm.env`
- Container image default:
  - `nvcr.io/nvidia/vllm:26.02-py3`

Useful installer env overrides:

- `CLAWCONTROL_VLLM_RUNTIME_MODE=auto|container|native`
- `CLAWCONTROL_VLLM_PORT=8000`
- `CLAWCONTROL_VLLM_GPU_MEMORY_UTILIZATION=0.9`
- `CLAWCONTROL_VLLM_TRUST_REMOTE_CODE=true|false`
- `CLAWCONTROL_VLLM_EXTRA_ARGS="..."`
- `CLAWCONTROL_HF_TOKEN=...`
- `CLAWCONTROL_HUGGING_FACE_HUB_TOKEN=...`

## Health checks and benchmarks

For `health_check` commands with `scope=model_benchmark`, the agent reports benchmark metrics including:

- `ttftMs`
- `outputTokensPerSec`
- `totalLatencyMs`
- `promptTokensPerSec`
- `p95TtftMsAtSmallConcurrency`

Profiles: `quick`, `standard`, `deep`.

## Troubleshooting

### Service is restarting / machine not checking in

```bash
sudo systemctl status clawcontrol-agent --no-pager -l
sudo journalctl -u clawcontrol-agent -n 200 --no-pager
sudo cat /etc/clawcontrol/agent.json
```

Common cause:

- `invalid config: server URL is required`
  - Fix `serverUrl` in `/etc/clawcontrol/agent.json`
  - Re-run installer with `--server ...`

### CLI command not found

Installer creates `clawcontrol` alias. If missing:

```bash
sudo ln -sf /usr/local/bin/clawcontrol-agent /usr/local/bin/clawcontrol
```

### Verify server reachability

```bash
curl -sS http://control.local:3400/api/health
```

### Verify current agent version

```bash
clawcontrol version
```

## Development

Build:

```bash
make build
```

Release assets:

```bash
make release
```
