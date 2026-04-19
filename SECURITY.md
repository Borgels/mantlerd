# Security and Trust Model

## What this document covers

`mantlerd` runs on your machine with elevated privileges and maintains a persistent connection to a remote control plane. This document explains:

- what the control plane is and is not allowed to ask the agent to do
- how command execution is bounded
- how to verify release binaries
- how to report a security vulnerability

## Architecture boundary

`mantlerd` is the on-machine agent. It does not contain the Mantler control-plane server. You configure the agent to point at a server URL (`--server` or `MANTLER_SERVER_URL`). By default this is the Mantler hosted service, but you can point it at any compatible server — including a self-hosted one.

This separation is intentional. You can inspect every command type, execution path, and network payload in this public repository.

## What the control plane can ask the agent to do

The agent receives typed commands from the server during check-ins (`/api/agent/checkin`). All recognized command types are handled in `internal/commands/executor.go`. Unknown command types are explicitly rejected.

Current command types fall into these categories:

**Runtime and model management**
- `install_runtime` / `uninstall_runtime` / `restart_runtime` — install or remove supported inference runtimes (Ollama, vLLM, TensorRT-LLM, etc.)
- `pull_model` / `start_model` / `stop_model` / `remove_model` / `build_model` — manage models on installed runtimes
- `install_tool` / `uninstall_tool` — install or remove host-level tooling (e.g. Docker, DCGM)

**Training**
- `install_trainer` / `uninstall_trainer` / `start_training` / `stop_training` / `training_status` / `export_checkpoint` — manage model fine-tuning jobs locally

**Harnesses and orchestrators**
- `run_harness_exec` — run a supported AI coding agent (Codex CLI, Goose, Claude Code, Aider, etc.) against a local task
- `harness_login` — authenticate a harness CLI tool using server-supplied credentials
- `sync_harnesses` — synchronise harness configuration
- `run_orchestrator_exec` — run an orchestrator binary (LangChain, AutoGen, etc.) with a server-supplied task

**Operations**
- `health_check` — report connectivity and runtime status to the server
- `model_eval` — run a local model evaluation suite
- `nccl_test` — run a distributed GPU connectivity test between cluster peers
- `cancel_command` — cancel an in-progress command

**Agent lifecycle**
- `update_agent` — download and install a newer agent binary from the GitHub release
- `self_shutdown` — shut down the machine (if the operator has configured the agent with that capability)
- `uninstall_agent` — remove the agent binary and service

## What the control plane cannot do

- Execute arbitrary shell commands. There is no `exec_shell` or equivalent command type; any unrecognised type is rejected.
- Access the local filesystem beyond the paths the agent explicitly manages (`/etc/mantler`, `~/.mantler`, `/var/lib/mantler`, and the configured model/runtime directories).
- Access other network services on your machine other than through the relay proxy to known runtime ports (Ollama, vLLM, etc.).
- Modify configuration files or system state outside of the managed runtime and model directories.

## Relay and pipeline traffic

The agent may receive relay messages over a WebSocket connection (`/api/agent/relay/ws`). These are used for two purposes:

1. **Localhost proxy requests** — forwarding inference API calls to local runtime backends (e.g. `http://127.0.0.1:11434`). Only requests to known runtime ports are processed.
2. **Pipeline stage requests** — encrypted, signed stage payloads for strategy pipeline execution. These are cryptographically gated: the agent generates a local Ed25519 stage keypair, and stage payloads must be encrypted to that key. The server cannot inject unsigned pipeline work.

## Local policy controls

You can restrict which remote commands the agent will execute by setting a `trustMode` in the agent config file.

**`managed`** (default): all command types are permitted. This is the existing behaviour and preserves backward compatibility.

**`restricted`**: commands in the destructive category (runtime installation, harness/orchestrator execution, agent update, shutdown, etc.) are denied unless you also list them in `allowedCommands`. Everything else continues to work normally.

Example config (`~/.mantler/agent.json` or `/etc/mantler/agent.json`):

```json
{
  "serverUrl": "https://control.example.com",
  "token": "…",
  "machineId": "…",
  "trustMode": "restricted",
  "allowedCommands": ["pull_model", "start_model", "stop_model"]
}
```

You can also disable the relay localhost proxy entirely:

```json
{
  "disableRelayProxy": true
}
```

## Audit log

The agent writes a newline-delimited JSON audit log of every server-originated command. Each line records the command type, outcome, whether it was destructive, and any denial/failure reason.

Default paths:
- Linux (root): `/var/log/mantler/audit.log`
- Linux (non-root) / macOS: `~/.mantler/audit.log`

View recent audit events with the CLI:

```bash
mantler audit              # last 50 events
mantler audit --lines 100
mantler audit --denied     # only denied or failed events
mantler audit --json       # raw JSON
```

## Release verification

Binary releases are published to [GitHub Releases](https://github.com/Borgels/mantlerd/releases) under the `Borgels/mantlerd` repository. Each release includes:

- `checksums.txt` — SHA-256 hashes for all platform binaries
- SLSA build provenance attestation generated by `actions/attest-build-provenance`

**Verify a binary manually (checksum):**

```bash
# Download the binary and checksum file
curl -LO https://github.com/Borgels/mantlerd/releases/download/vX.Y.Z/mantlerd-linux-amd64
curl -LO https://github.com/Borgels/mantlerd/releases/download/vX.Y.Z/checksums.txt

# Verify
grep mantlerd-linux-amd64 checksums.txt | sha256sum --check
```

**Verify SLSA provenance (requires the `gh` CLI):**

```bash
gh attestation verify mantlerd-linux-amd64 --owner Borgels
```

The `mantler update apply` command and the daemon's server-driven `update_agent` path both verify the SHA-256 checksum before replacing the running binary. Neither path pipes a remote shell script.

## Local socket and LAN transfer boundaries

**Unix domain socket:** The daemon creates a local control socket that the `mantler` CLI uses to delegate model-pull and runtime-install operations to the already-running daemon (which may have elevated privileges). Access is controlled by OS filesystem permissions: the socket is created with mode `0660` and, on Linux when running as root, owned by `root:mantler` so that members of the `mantler` group can connect without root.

To disable the socket entirely:

```json
{
  "disableLocalSocket": true
}
```

**LAN model transfer server:** When `allowModelSharing` is `true`, the agent binds a transfer server on port 7433 on all interfaces. Each request must carry an HMAC-signed token distributed by the control plane. This service should only be enabled when you actually need cross-machine model sharing. When `allowModelSharing` is `false` (the default), the port is not opened.

To verify the current LAN exposure:

```bash
mantler info     # shows whether model sharing is active
ss -tlnp | grep 7433  # confirm whether the port is listening
```

## Reporting a vulnerability

Please do not report security vulnerabilities through public GitHub issues.

Email **security@mantler.ai** with:
- a description of the vulnerability
- steps to reproduce
- the version of `mantlerd` you are running (`mantler version`)
- your assessment of severity and exploitability

We aim to acknowledge reports within 48 hours and provide a fix timeline within 7 days for critical issues.

Security reports are credited in the release notes unless you prefer to remain anonymous.
