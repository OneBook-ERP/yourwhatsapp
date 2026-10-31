.PHONY: build test run

build:
	CGO_ENABLED=0 go build -o bin/relay ./cmd/relay

test:
	go test ./...

run:
	go run ./cmd/relay
