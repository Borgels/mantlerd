# mantlerd

Mantler machine daemon for runtime and model orchestration.

The daemon runs on each worker machine, checks in with Mantler, executes typed commands, and reports runtime/model state back to the control plane.

## Overview

`mantlerd` does three core things:

- Performs periodic authenticated check-ins to Mantler (`/api/agent/checkin`)
- Executes allowlisted commands returned by the server
- Acknowledges command results (`/api/agent/ack`)

## Installation (Linux, recommended)

Use the release installer script:

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/mantlerd/master/scripts/install.sh | \
  sudo sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server https://control.example.com
```

For non-HTTPS endpoints:

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/mantlerd/master/scripts/install.sh | \
  sudo sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server http://control.local:3400 \
  --insecure
```

### What the installer sets up

- Installs `/usr/local/bin/mantlerd`
- Creates CLI command `/usr/local/bin/mantler` (symlink)
- Writes config file `/etc/mantler/agent.json` (`0600`)
- Installs systemd unit `/etc/systemd/system/mantlerd.service`
- Starts daemon with:
  - `ExecStart=/usr/local/bin/mantlerd start --config /etc/mantler/agent.json`

Legacy `CLAWCONTROL_*` installer environment variables are still accepted for compatibility during migration.

## Configuration

Default config path:

- root: `/etc/mantler/agent.json` (fallback: `/etc/clawcontrol/agent.json`)
- non-root: `~/.mantler/agent.json` (fallback: `~/.clawcontrol/agent.json`)

Manage config via CLI:

```bash
mantler config show
mantler config path
mantler config set server https://control.example.com
mantler config set interval 30s
```

## CLI quick reference

```bash
mantler --help
mantler version
mantler update check
mantler update apply --yes
mantler doctor
mantler info
mantler start
mantler checkin
mantler runtime list
mantler model list
```

## Development

```bash
make build
make release
```
