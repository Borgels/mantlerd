APP_NAME=clawcontrol-agent

.PHONY: build
build:
	go build -o bin/$(APP_NAME) ./cmd/$(APP_NAME)

.PHONY: run
run:
	go run ./cmd/$(APP_NAME) --help

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: release
release:
	chmod +x scripts/release-build.sh
	scripts/release-build.sh "$$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
