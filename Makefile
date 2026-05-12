SHELL := /usr/bin/env bash

BIN        := llmtap
PKG        := github.com/colinedwardwood/llmtap
CMD        := ./cmd/llmtap
DIST       := dist

VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X $(PKG)/internal/buildinfo.Version=$(VERSION) \
  -X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
  -X $(PKG)/internal/buildinfo.Date=$(BUILD_DATE)

GO       ?= go
GOFLAGS  ?= -trimpath
TESTOPTS ?= -race -count=1 -timeout=60s

.PHONY: all build run test lint vet tidy fmt clean docker docker-build docker-push compose-up compose-up-insecure compose-down release

all: lint test build

build:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) $(CMD)

run: build
	./$(BIN) up --config config.yaml

test:
	$(GO) test $(TESTOPTS) ./...

vet:
	$(GO) vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; running 'go vet' as a fallback"; \
		$(GO) vet ./...; \
	fi

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

clean:
	rm -rf $(BIN) $(DIST) coverage.txt coverage.html

# Cross-compile a release set (linux/amd64, linux/arm64, darwin/arm64).
release: clean
	@mkdir -p $(DIST)
	@for target in linux/amd64 linux/arm64 darwin/arm64; do \
	  os=$${target%%/*}; arch=$${target##*/}; \
	  out=$(DIST)/$(BIN)-$$os-$$arch; \
	  echo ">> $$out"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(CMD); \
	done

docker-build:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t llmtap:$(VERSION) -t llmtap:latest .

# Brings up the demo stack with llmtap listening on HTTPS :4443. The
# self-signed cert is generated on first run by the tls-init sidecar.
# Clients point OPENAI_BASE_URL=https://localhost:4443/v1 and either
# trust the cert or pass `curl -k`.
compose-up:
	docker compose -f deploy/compose/docker-compose.yml up -d --build

# Plaintext-loopback escape hatch — only for a 30-second
# `curl http://localhost:4000/v1/...` demo. Teaches the wrong trust
# model for production. Prefer `compose-up` everywhere else.
compose-up-insecure:
	docker compose \
	  -f deploy/compose/docker-compose.yml \
	  -f deploy/compose/docker-compose.insecure.yml \
	  up -d --build

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down -v
