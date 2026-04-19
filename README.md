# mantlerd

[![CI](https://github.com/Borgels/mantlerd/actions/workflows/ci.yml/badge.svg)](https://github.com/Borgels/mantlerd/actions/workflows/ci.yml)
[![CodeQL](https://github.com/Borgels/mantlerd/actions/workflows/codeql.yml/badge.svg)](https://github.com/Borgels/mantlerd/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/Borgels/mantlerd)](https://github.com/Borgels/mantlerd/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/Borgels/mantlerd)](https://goreportcard.com/report/github.com/Borgels/mantlerd)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Status: initial public release track (`v0.6.x`).

`mantlerd` is the machine daemon that checks into Mantler, executes allowlisted commands, and reports runtime/model/machine state.

```text
At a glance
- What this repo is: the open-source edge agent that runs on worker machines.
- What it is not: the Mantler web control plane or hosted API gateway.
- Core flow: check-in -> command execution -> ack/report.
- Try it locally: make build && ./bin/mantlerd start --config ./examples/agent.json
- Quick CLI: mantler --help
```

## Quickstart

```bash
curl -sSL https://raw.githubusercontent.com/Borgels/mantlerd/master/scripts/install.sh | \
  sudo sh -s -- --token YOUR_MACHINE_TOKEN --machine MACHINE_ID --server https://control.example.com
```

## Links

- [Docs home](https://docs.mantler.ai)
- [Machine install](https://docs.mantler.ai/machines/install)
- [Machine configuration](https://docs.mantler.ai/machines/configuration)
- [Machine security](https://docs.mantler.ai/machines/security)
- [Machine CLI](https://docs.mantler.ai/machines/cli)

## Development

```bash
make build
make hooks
go test -race -count=1 ./...
```

## Security

See [SECURITY.md](SECURITY.md) for command model, trust boundaries, and release verification.

## License

[MIT](LICENSE)
