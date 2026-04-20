BINARY   := crucible
AGENT    := bin/crucible-agent
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/gnana997/crucible/internal/version.version=$(VERSION)

# Rootfs build parameters — override on the command line.
#   make rootfs BASE_ROOTFS=assets/ubuntu.squashfs OUT_ROOTFS=assets/rootfs.ext4
BASE_ROOTFS ?=
OUT_ROOTFS  ?= assets/rootfs.ext4
ROOTFS_SIZE ?= 1G

.PHONY: all build agent rootfs test race vet fmt lint tidy clean help

all: fmt vet test build

help:
	@echo "targets:"
	@echo "  build    - build the crucible daemon binary"
	@echo "  agent    - build the guest agent (linux/amd64, static)"
	@echo "  rootfs   - bake agent into an ext4 rootfs (needs BASE_ROOTFS=...)"
	@echo "  test     - go test ./..."
	@echo "  race     - go test -race ./..."
	@echo "  vet      - go vet ./..."
	@echo "  fmt      - gofmt -s -w ."
	@echo "  lint     - golangci-lint run (requires golangci-lint)"
	@echo "  tidy     - go mod tidy"
	@echo "  clean    - remove built binaries"

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/crucible

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

tidy:
	go mod tidy

clean:
	rm -f $(BINARY) $(AGENT)
