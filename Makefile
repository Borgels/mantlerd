APP_NAME=mantlerd
CMD_DIR=clawcontrol-agent

.PHONY: build
build:
	go build -o bin/$(APP_NAME) ./cmd/$(CMD_DIR)

.PHONY: run
run:
	go run ./cmd/$(CMD_DIR) --help

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: release
release:
	chmod +x scripts/release-build.sh
	scripts/release-build.sh "$$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
