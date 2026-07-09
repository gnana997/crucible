BINARY   := crucible
AGENT    := bin/crucible-agent
EMBEDDED_AGENT := internal/agentbin/crucible-agent
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/gnana997/crucible/internal/version.version=$(VERSION)

# Rootfs build parameters — override on the command line.
#   make rootfs BASE_ROOTFS=assets/ubuntu.squashfs OUT_ROOTFS=assets/rootfs.ext4
BASE_ROOTFS ?=
OUT_ROOTFS  ?= assets/rootfs.ext4
ROOTFS_SIZE ?= 1G

# Cross-platform client build. The daemon subcommand is Linux-only (it needs
# KVM/Firecracker) and stubs out elsewhere, so the CLI + `mcp serve` build for
# macOS/Windows too. Override CLIENT_GOOS/CLIENT_GOARCH; default to the host.
DIST          ?= dist
CLIENT_GOOS   ?= $(shell go env GOOS)
CLIENT_GOARCH ?= $(shell go env GOARCH)
CLIENT_EXT    := $(if $(filter windows,$(CLIENT_GOOS)),.exe,)
CLIENT_PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all build bench client client-all agent rootfs profile test race vet fmt lint tidy clean hooks help

all: fmt vet test build

help:
	@echo "targets:"
	@echo "  build    - build the crucible daemon binary"
	@echo "  client   - cross-build the client CLI (CLIENT_GOOS/CLIENT_GOARCH; default host)"
	@echo "  client-all - cross-build the client for every CLIENT_PLATFORMS target"
	@echo "  agent    - build the guest agent (linux/amd64, static)"
	@echo "  rootfs   - bake agent into an ext4 rootfs (needs BASE_ROOTFS=...)"
	@echo "  profile  - build a native language rootfs profile (needs PROFILE=..., docker)"
	@echo "  test     - go test ./..."
	@echo "  race     - go test -race ./..."
	@echo "  vet      - go vet ./..."
	@echo "  fmt      - gofmt -s -w ."
	@echo "  lint     - golangci-lint run (requires golangci-lint)"
	@echo "  hooks    - install git pre-commit/pre-push hooks (requires lefthook)"
	@echo "  tidy     - go mod tidy"
	@echo "  clean    - remove built binaries"

# build embeds the freshly-built guest agent into the daemon (via the
# `embedagent` tag) so converted OCI images boot a version-matched
# agent. The agent is copied into the agentbin package first because
# go:embed can't reach outside its own directory. Plain `go build ./...`
# (no tag) uses the stub and needs no embedded binary.
build: agent
	cp $(AGENT) $(EMBEDDED_AGENT)
	go build -tags embedagent -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/crucible

# Benchmark harness (drives a running daemon through internal/client).
bench:
	mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/crucible-bench ./cmd/crucible-bench

# Cross-build the client CLI (CLI + `mcp serve`) for one target. No embedagent
# tag: the embedded agent is only used by the daemon's image conversion, which
# doesn't exist off-Linux.
#   make client CLIENT_GOOS=darwin CLIENT_GOARCH=arm64
client:
	@mkdir -p $(DIST)
	@echo "building client $(CLIENT_GOOS)/$(CLIENT_GOARCH) -> $(DIST)/$(BINARY)_$(CLIENT_GOOS)_$(CLIENT_GOARCH)$(CLIENT_EXT)"
	GOOS=$(CLIENT_GOOS) GOARCH=$(CLIENT_GOARCH) CGO_ENABLED=0 \
	    go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)_$(CLIENT_GOOS)_$(CLIENT_GOARCH)$(CLIENT_EXT) ./cmd/crucible

# Cross-build the client for every target in CLIENT_PLATFORMS.
client-all:
	@for p in $(CLIENT_PLATFORMS); do \
	    $(MAKE) --no-print-directory client CLIENT_GOOS=$${p%/*} CLIENT_GOARCH=$${p#*/} || exit 1; \
	done

# The guest agent always runs inside a Linux microVM, so we pin the
# target triple and build statically to avoid libc surprises inside
# different rootfs distros.
agent:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	    go build -ldflags '$(LDFLAGS)' -o $(AGENT) ./cmd/crucible-agent
	@file $(AGENT) 2>/dev/null || true

rootfs: agent
	@if [ -z "$(BASE_ROOTFS)" ]; then \
	    echo "error: BASE_ROOTFS is required (path to ubuntu*.squashfs)" >&2; \
	    exit 2; \
	fi
	mkdir -p $(dir $(OUT_ROOTFS))
	./scripts/build-rootfs.sh $(BASE_ROOTFS) $(AGENT) $(OUT_ROOTFS) $(ROOTFS_SIZE)

# Build a native language rootfs profile from profiles/profiles.env, e.g.
#   make profile PROFILE=python-3.12
# Needs docker; output lands in assets/profiles/<PROFILE>.ext4.
profile: agent
	@if [ -z "$(PROFILE)" ]; then \
	    echo "error: PROFILE is required (e.g. make profile PROFILE=python-3.12)" >&2; \
	    exit 2; \
	fi
	./scripts/build-profile.sh $(PROFILE)

test:
	go test $(PKG)

race:
	go test -race $(PKG)

vet:
	go vet $(PKG)

fmt:
	gofmt -s -w .

lint:
	golangci-lint run

# Install the git hooks defined in lefthook.yml (fmt / vet / lint / build on
# commit, race tests on push). One-time; requires lefthook on PATH.
hooks:
	lefthook install

tidy:
	go mod tidy

clean:
	rm -f $(BINARY) $(AGENT) $(EMBEDDED_AGENT)
	rm -rf $(DIST)
