.PHONY: web
web: ## Build the embedded console SPA into web/dist
	bash web/scripts/build.sh

.PHONY: build
build: web ## Build the node binary (console embedded)
	go build ./...
