.PHONY: build test test-hooks lint lint-osrename install clean help

# Build variables
BINARY_NAME=claudecm
VERSION?=dev
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X github.com/a2d2-dev/claudecm/cmd.Version=$(VERSION) -X github.com/a2d2-dev/claudecm/cmd.Commit=$(COMMIT) -X github.com/a2d2-dev/claudecm/cmd.Date=$(BUILD_DATE)"

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	go build $(LDFLAGS) -o bin/$(BINARY_NAME) .
	@echo "✓ Binary built: bin/$(BINARY_NAME)"

install: ## Install the binary to $GOPATH/bin
	@echo "Installing $(BINARY_NAME)..."
	go install $(LDFLAGS) .
	@echo "✓ Installed to $(shell go env GOPATH)/bin/$(BINARY_NAME)"

test: ## Run tests
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...
	@echo "✓ Tests completed"

test-hooks: ## Run tests that require the build-tagged test seams (e.g. atomic write fsync-error injection)
	@echo "Running tests with -tags=test..."
	go test -count=1 -tags=test ./internal/storage/...
	@echo "✓ Test-tagged tests completed"

test-coverage: test ## Run tests with coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "✓ Coverage report: coverage.html"

lint: lint-osrename ## Run linters
	@echo "Running linters..."
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found. Installing..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	}
	golangci-lint run ./...
	@echo "✓ Linting completed"

lint-osrename: ## Enforce Story E2-S2 AC #5 os.Rename discipline
	@bash scripts/lint-osrename.sh

fmt: ## Format code
	@echo "Formatting code..."
	gofmt -s -w .
	@echo "✓ Code formatted"

vet: ## Run go vet
	@echo "Running go vet..."
	go vet ./...
	@echo "✓ Vet completed"

clean: ## Clean build artifacts
	@echo "Cleaning..."
	rm -rf bin/
	rm -f coverage.out coverage.html
	@echo "✓ Cleaned"

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy
	@echo "✓ Dependencies downloaded"

run: ## Run the application
	go run $(LDFLAGS) . $(ARGS)

dev-build: fmt vet test build ## Run fmt, vet, test, and build

all: clean deps lint test build ## Run all checks and build

.DEFAULT_GOAL := help
