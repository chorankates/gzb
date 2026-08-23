.PHONY: all test build run fuzz

all: test build

test:
	@test -z "$$(gofmt -l .)"
	@go vet ./...
	@go test -race ./...

build:
	@go build -o gzb ./cmd/gzb

run:
	@go run ./cmd/gzb

fuzz:
	@go test -fuzz=FuzzDecode -fuzztime=10s ./internal/ash
	@go test -fuzz=FuzzDecodeMessage -fuzztime=10s ./internal/ezsp
	@go test -fuzz=FuzzDecodeAndAttributes -fuzztime=10s ./internal/zcl
	@go test -fuzz=FuzzDecodeResponses -fuzztime=10s ./internal/zcl
	@go test -fuzz=FuzzParseResponses -fuzztime=10s ./internal/zdo
