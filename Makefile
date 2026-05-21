# open-artifact convenience targets. The release pipeline runs goreleaser
# via GitHub Actions; these are for local dev.

.PHONY: build test test-integration lint tidy snapshot help

build: ## Build both binaries into ./bin
	go build -o bin/open-artifact ./cmd/server
	go build -o bin/artctl ./cmd/artctl

test: ## Run unit tests with the race detector
	go test -race ./...

test-integration: ## Run unit + integration tests (memblob/fileblob backends)
	go test -race -tags=integration ./...

lint: ## Vet the module
	go vet ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

snapshot: ## Build local-only release artifacts without publishing
	goreleaser release --snapshot --clean --skip=publish

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'
