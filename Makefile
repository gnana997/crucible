BINARY   := crucible
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/gnana997/crucible/internal/version.version=$(VERSION)

.PHONY: all build test race vet fmt lint tidy clean help

all: fmt vet test build

help:
	@echo "targets: build test race vet fmt lint tidy clean"

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/crucible

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
	rm -f $(BINARY)
