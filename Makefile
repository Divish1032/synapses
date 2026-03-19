.PHONY: build test lint clean install fmt vet \
        run/index run/start run/status run/reset run/reset-all

BINARY     := synapses
BUILD_DIR  := bin
CMD_PATH   := ./cmd/synapses

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-X main.version=$(VERSION) -s -w"

## build: Compile the binary
build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD_PATH)

INSTALL_DIR := $(HOME)/.synapses/bin

## install: Build and install binary to ~/.synapses/bin
install: build
	@mkdir -p $(INSTALL_DIR)
	@cp $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed $(INSTALL_DIR)/$(BINARY)"

## test: Run all tests
test:
	go test ./... -race -count=1 -timeout 300s

## test/cover: Run tests with coverage report (excludes CLI-only packages: bicep_edge, debug_callsites)
test/cover:
	go test ./internal/... ./cmd/synapses -coverprofile=coverage.out -covermode=atomic -race -timeout 300s
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1
	@echo "Coverage report: coverage.html (excludes cmd/bicep_edge, cmd/debug_callsites)"

## test/cover/pkg: Show per-package coverage summary (excludes CLI-only packages: bicep_edge, debug_callsites)
test/cover/pkg:
	@go test ./internal/... ./cmd/synapses -cover -timeout 300s 2>&1 | grep -E "^(ok|FAIL|---)" | sort

## lint: Run golangci-lint (requires: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
lint:
	golangci-lint run ./...

## fmt: Format all Go source files
fmt:
	gofmt -s -w .

## vet: Run go vet
vet:
	go vet ./...

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

## tidy: Tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

# --- Shortcut run targets (pass PATH=<dir> to target a different repo) ---
REPO_PATH ?= .

## run/index: Build and index the current repo (REPO_PATH=. by default)
run/index: build
	./$(BUILD_DIR)/$(BINARY) index -path $(REPO_PATH)

## run/start: Build and start the MCP server for the current repo
run/start: build
	./$(BUILD_DIR)/$(BINARY) start -path $(REPO_PATH)

## run/status: Show index statistics for the current repo
run/status: build
	./$(BUILD_DIR)/$(BINARY) status -path $(REPO_PATH)

## run/reset: Remove the index for the current repo
run/reset: build
	./$(BUILD_DIR)/$(BINARY) reset -path $(REPO_PATH)

## run/reset-all: Remove ALL cached indexes
run/reset-all: build
	./$(BUILD_DIR)/$(BINARY) reset -all

## help: Print this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
