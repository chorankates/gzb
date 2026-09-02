.PHONY: all test build run fuzz recapture recapture-light

all: test build

test:
	@test -z "$$(gofmt -l .)"
	@go vet ./...
	@go test -race ./...

build:
	@go build -o gzb ./cmd/gzb

run:
	@go run ./cmd/gzb

repl: build
	@go run ./cmd/gzb repl

# Re-captures the console transcripts in README.md from a real device. Needs
# hardware and a device to aim at, which is why it is not part of `test`:
#   make recapture DEVICE="living room thermo"
#   make recapture DEVICE="living room thermo" CONFIGURE=--configure
#
# The transcript is written to $(RECAPTURE_OUT) as well as to the terminal, so
# it can be read back from disk instead of copied out of a scrollback.
RECAPTURE_OUT ?= recapture.md
recapture: build
	@OUT="$(RECAPTURE_OUT)" ./scripts/recapture.sh "$(DEVICE)" $(CONFIGURE)

# The light half of the same idea. Nothing it runs turns a light on, so it is
# safe against a light that is meant to stay dark:
#   make recapture-light LIGHT=light1
#   make recapture-light LIGHT=light1 CONFIGURE=--configure
RECAPTURE_LIGHT_OUT ?= recapture-light.md
recapture-light: build
	@OUT="$(RECAPTURE_LIGHT_OUT)" ./scripts/recapture-light.sh "$(LIGHT)" $(CONFIGURE)

fuzz:
	@go test -fuzz=FuzzDecode -fuzztime=10s ./internal/ash
	@go test -fuzz=FuzzDecodeMessage -fuzztime=10s ./internal/ezsp
	@go test -fuzz=FuzzDecodeAndAttributes -fuzztime=10s ./internal/zcl
	@go test -fuzz=FuzzDecodeResponses -fuzztime=10s ./internal/zcl
	@go test -fuzz=FuzzParseResponses -fuzztime=10s ./internal/zdo
