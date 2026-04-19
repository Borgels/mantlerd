# mantlerd

[![CI](https://github.com/Borgels/mantlerd/actions/workflows/ci.yml/badge.svg)](https://github.com/Borgels/mantlerd/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Mantler machine daemon for runtime and model orchestration.

The daemon runs on each worker machine, checks in with Mantler, executes typed commands, and reports runtime/model state back to the control plane.

## Documentation

- [Docs home](https://docs.mantler.ai)
- [Machine install](https://docs.mantler.ai/machines/install)
- [Machine configuration](https://docs.mantler.ai/machines/configuration)
- [Machine security](https://docs.mantler.ai/machines/security)
- [Machine CLI](https://docs.mantler.ai/machines/cli)

## Architecture: public agent and private control plane

`mantlerd` is the **open-source edge half** of the Mantler system. The other half — the Mantler control plane — is a separate, private server that is not part of this repository.

```
┌─────────────────────────────┐        ┌───────────────────────────────────┐
│  Worker machine (this repo) │        │  Mantler control plane (private)  │
│                             │        │                                   │
│  mantlerd daemon            │◄──────►│  /api/agent/checkin               │
│   - runtime orchestration   │  HTTPS │  /api/agent/ack                   │
│   - model management        │        │  /api/recommendations             │
│   - strategy pipeline exec  │        │  /api/agent/relay/ws (WebSocket)  │
│   - local hardware access   │        │                                   │
└─────────────────────────────┘        └───────────────────────────────────┘
```

**Why this repo is public:** `mantlerd` runs on your machine with elevated privileges. Transparency matters. You can read every check-in payload, command type, and execution path in this codebase. The agent connects to any HTTP(S) endpoint you configure — you are not required to use Mantler's hosted service.

**What lives in the private repo:** the control-plane server implementation, fleet management, recommendation and scoring data, hosted relay, and product UI. None of that code ships in this agent.

See [SECURITY.md](SECURITY.md) for details on what the control plane is and is not allowed to ask the agent to do, and how to verify releases.

## Overview

`mantlerd` does three core things:

- Performs periodic authenticated check-ins to Mantler (`/api/agent/checkin`)
- Executes allowlisted commands returned by the server
- Acknowledges command results (`/api/agent/ack`)

## Strategy pipeline execution role

`mantlerd` is the stage execution agent for strategy pipelines.

For pipeline stage requests, the daemon:

- Decrypts inbound stage envelopes locally.
- Executes `compress` or `infer` stage work against local runtime backends.
- Validates compression contract structure for `CompressedContext`.
- Emits signed `StageIntegrity` sidecars (`Ed25519`) with hash/token/runtime metadata.
- Re-encrypts continuation payloads for the next stage target.

Current runtime stage flow supports:

- single infer stage
- two-stage `compress -> infer` pipeline

## Pipeline hardening notes

- Stage keypair lifecycle is generated locally (`EnsureStageKeys`), reported in check-ins when available, and omitted from payloads on initialization failure.
- Stage processing honors relay timeout budgets (`TimeoutMs`) to avoid unbounded execution.
- Runtime port resolution is centralized to prevent drift across relay/runtime paths.
- Request and response buffers use best-effort memory zeroing for sensitive bytes.

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

## Installation (macOS)

Use the same release installer script (no `sudo` required for service setup):

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/mantlerd/master/scripts/install.sh | \
  sh -s -- \
  --token YOUR_MACHINE_TOKEN \
  --machine MACHINE_ID \
  --server http://control.local:3400
```

On macOS, the installer:

- Installs `/usr/local/bin/mantlerd` (and `/usr/local/bin/mantler` symlink)
- Writes config to `~/.mantler/agent.json`
- Installs a user launchd agent at `~/Library/LaunchAgents/com.mantler.mantlerd.plist`
- Starts the daemon via launchd

Check status with:

```bash
launchctl print "gui/$(id -u)/com.mantler.mantlerd"
```

### What the installer sets up

- Installs `/usr/local/bin/mantlerd`
- Creates CLI command `/usr/local/bin/mantler` (symlink)
- Writes config file `/etc/mantler/agent.json` (`0600`)
- Installs systemd unit `/etc/systemd/system/mantlerd.service`
- Starts daemon with:
  - `ExecStart=/usr/local/bin/mantlerd start --config /etc/mantler/agent.json`

## Configuration

Default config path:

- root: `/etc/mantler/agent.json` (fallback: `/etc/mantler/agent.json`)
- non-root: `~/.mantler/agent.json` (fallback: `~/.mantler/agent.json`)

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

## License

[MIT](LICENSE) — Copyright (c) 2026 Borgels Olsen Holding ApS
