.PHONY: help build test race vet fmt fmt-check lint tidy cover cross clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-12s %s\n", $$1, $$2}'

build: ## Build all packages
	go build ./...

test: ## Run unit tests
	go test -count=1 ./...

race: ## Run unit tests with the race detector
	go test -race -count=1 ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

fmt-check: ## Fail if any file needs formatting
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then \
		echo "gofmt required for:"; echo "$$unformatted"; exit 1; fi

lint: ## Run golangci-lint (requires golangci-lint v2)
	golangci-lint run ./...

tidy: ## Verify go.mod/go.sum are tidy
	go mod tidy

cover: ## Run tests with coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

cross: ## Cross-compile for all supported platforms
	@for target in linux/amd64 linux/arm linux/arm64 windows/amd64 darwin/arm64 freebsd/amd64; do \
		echo "building $$target"; \
		GOOS=$${target%/*} GOARCH=$${target#*/} go build ./... || exit 1; \
	done

clean: ## Remove build artifacts
	rm -f coverage.out
