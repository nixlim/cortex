# Cortex build targets
#
# Defaults target darwin/arm64 as required by Phase 1 (FR / cortex-4kq.10).
# Builds are CGO-free and reproducible: -trimpath strips local paths,
# -buildvcs=false drops embedded VCS state, and -buildid= clears the Go
# build ID so two clean builds from the same commit produce byte-identical
# binaries.

BINARY        := bin/cortex
MODULE        := github.com/nixlim/cortex
VERSION_PKG   := $(MODULE)/internal/version
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GOFLAGS_COMMON := -trimpath -buildvcs=false
LDFLAGS        := -s -w -buildid= -X $(VERSION_PKG).Version=$(VERSION)

GOOS_RELEASE   ?= darwin
GOARCH_RELEASE ?= arm64

.PHONY: all build release release-verify clean test test-cli vet tidy

all: build

## build: Host-native debug build (convenience, not a release artifact).
build:
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS_COMMON) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/cortex

## release: Reproducible darwin/arm64 single-binary release build.
release:
	@GOOS_RELEASE=$(GOOS_RELEASE) GOARCH_RELEASE=$(GOARCH_RELEASE) VERSION=$(VERSION) ./scripts/build-release.sh

## release-verify: Build release twice and assert byte-identical output.
release-verify: release
	@cp $(BINARY) bin/cortex.first
	@rm -f $(BINARY)
	@$(MAKE) --no-print-directory release
	@cmp -s bin/cortex.first $(BINARY) && echo "OK: reproducible build" || (echo "FAIL: release builds differ"; exit 1)
	@rm -f bin/cortex.first

## clean: Remove build artifacts.
clean:
	rm -rf bin/

## test: Run all Go tests.
test:
	go test ./...

## test-cli: Run the CLI-exec harness (builds the cortex binary and
## invokes subcommands as real subprocesses). Gated behind the `cli`
## build tag so it stays out of `make test`. Stub canary tests in this
## suite are expected to fail until each notImplemented stub in
## cmd/cortex/commands.go is replaced with real wiring; every red line
## is a CLI verb that is documented but not yet honest.
test-cli:
	go test -tags=cli ./internal/e2e/...

## vet: Run go vet across all packages.
vet:
	go vet ./...

## tidy: Normalize go.mod/go.sum.
tidy:
	go mod tidy
