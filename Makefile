# Release version stamped into the binary (proto-compat version the Agency gates
# on). The most recent tag, v-stripped so it matches the Agency's ExpectedVersion
# (e.g. tag v0.1.3 -> "0.1.3"). Falls back to "dev" with no tags or no git. A bare
# `go run`/`go build ./...` keeps main.version's "dev" default (dev nodes drift).
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo dev)

.PHONY: web
web: ## Build the embedded console SPA into web/dist
	bash web/scripts/build.sh

.PHONY: build
build: web ## Build the node binary (console embedded, version stamped)
	go build ./...
	go build -ldflags "-X main.version=$(VERSION)" -o sbx-swarm-node ./cmd/sbx-swarm-node
