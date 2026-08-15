.PHONY: test run

all: test build

test:
	@go fmt
	@go test ./...
	@go vet ./...

build:
	@go build -o gzb ./cmd/gzb

run:
	@go run ./cmd/gzb
