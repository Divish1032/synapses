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

## install: Install binary to GOPATH/bin
install:
	go install $(LDFLAGS) $(CMD_PATH)

## test: Run all tests
test:
	go test ./... -v -race -timeout 60s

## test/cover: Run tests with coverage report
test/cover:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

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
