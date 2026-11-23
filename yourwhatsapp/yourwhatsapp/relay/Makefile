.PHONY: build test run

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

build:
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o bin/relay ./cmd/relay

test:
	go test ./...

run:
	go run ./cmd/relay
