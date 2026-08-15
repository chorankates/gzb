.PHONY: test run

all: test build

test:
	@go test ./...

build:
	@go build -o gzb ./cmd/gzb

run:
	@go run ./cmd/gzb
