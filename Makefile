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
	GOOS=linux GOARCH=amd64 go build -o dist/$(APP_NAME)-linux-amd64 ./cmd/$(APP_NAME)
	GOOS=linux GOARCH=arm64 go build -o dist/$(APP_NAME)-linux-arm64 ./cmd/$(APP_NAME)
	GOOS=darwin GOARCH=amd64 go build -o dist/$(APP_NAME)-darwin-amd64 ./cmd/$(APP_NAME)
	GOOS=darwin GOARCH=arm64 go build -o dist/$(APP_NAME)-darwin-arm64 ./cmd/$(APP_NAME)
