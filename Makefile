.PHONY: build test lint clean install fmt vet completions \
        run/index run/start run/status run/reset run/reset-all \
        loadtest loadtest/generate

BINARY     := synapses
BUILD_DIR  := bin
CMD_PATH   := ./cmd/synapses

VERSION    ?= $(shell git describe --tags --always --dirty --match "v[0-9]*" 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-X main.version=$(VERSION) -s -w"

## build: Compile Go binary
build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD_PATH)

## install: Build and install synapses binary
##
## Strategy: use `go install` so the binary lands in GOBIN/GOPATH (already in
## PATH for any Go developer). Also copy to ~/.synapses/bin/ for the Tauri
## desktop app's find_binary() fallback.
##
## Works immediately — no terminal restart needed on macOS or Linux.
install: build
	@go install $(LDFLAGS) $(CMD_PATH)
	@GOBIN_DIR=$$(go env GOBIN); \
	GOPATH_DIR=$$(go env GOPATH); \
	EFFECTIVE_BIN=$${GOBIN_DIR:-$${GOPATH_DIR}/bin}; \
	echo "  Installed $$EFFECTIVE_BIN/$(BINARY)"; \
	mkdir -p "$$HOME/.synapses/bin"; \
	cp "$(BUILD_DIR)/$(BINARY)" "$$HOME/.synapses/bin/$(BINARY)" 2>/dev/null || true; \
	xattr -d com.apple.quarantine "$$HOME/.synapses/bin/$(BINARY)" 2>/dev/null || true; \
	codesign --force --sign - "$$HOME/.synapses/bin/$(BINARY)" 2>/dev/null || true; \
	if command -v $(BINARY) >/dev/null 2>&1; then \
		printf "  \033[32m✓\033[0m Ready! Run: synapses version\n"; \
	else \
		echo ""; \
		printf "  \033[33m!\033[0m $$EFFECTIVE_BIN is not in your PATH.\n"; \
		echo ""; \
		echo "  Activate now (pick one):"; \
		echo "    export PATH=\"$$EFFECTIVE_BIN:\$$PATH\""; \
		echo ""; \
		echo "  Persist for future sessions — add to your shell config:"; \
		SHELL_NAME=$$(basename "$$SHELL" 2>/dev/null); \
		case "$$SHELL_NAME" in \
			zsh)  echo "    echo 'export PATH=\"$$EFFECTIVE_BIN:\$$PATH\"' >> ~/.zshrc" ;; \
			bash) \
				if [ -f "$$HOME/.bash_profile" ]; then \
					echo "    echo 'export PATH=\"$$EFFECTIVE_BIN:\$$PATH\"' >> ~/.bash_profile"; \
				else \
					echo "    echo 'export PATH=\"$$EFFECTIVE_BIN:\$$PATH\"' >> ~/.bashrc"; \
				fi ;; \
			fish) echo "    fish_add_path $$EFFECTIVE_BIN" ;; \
			*)    echo "    echo 'export PATH=\"$$EFFECTIVE_BIN:\$$PATH\"' >> ~/.profile" ;; \
		esac; \
		echo ""; \
	fi

## test: Run all tests
test:
	go test ./... -race -count=1 -timeout 600s

## test/cover: Run tests with coverage report (excludes CLI-only packages: bicep_edge, debug_callsites)
test/cover:
	go test ./internal/... ./cmd/synapses -coverprofile=coverage.out -covermode=atomic -race -timeout 600s
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1
	@echo "Coverage report: coverage.html (excludes cmd/bicep_edge, cmd/debug_callsites)"

## test/cover/pkg: Show per-package coverage summary (excludes CLI-only packages: bicep_edge, debug_callsites)
test/cover/pkg:
	@go test ./internal/... ./cmd/synapses -cover -timeout 600s 2>&1 | grep -E "^(ok|FAIL|---)" | sort

## lint: Run golangci-lint (requires: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
lint:
	golangci-lint run ./...

## fmt: Format all Go source files
fmt:
	gofmt -s -w .

## vet: Run go vet
vet:
	go vet ./...

## completions: Generate shell completion scripts for release archives
completions: build
	@mkdir -p completions
	./$(BUILD_DIR)/$(BINARY) completion bash > completions/synapses.bash
	./$(BUILD_DIR)/$(BINARY) completion zsh  > completions/synapses.zsh
	./$(BUILD_DIR)/$(BINARY) completion fish > completions/synapses.fish
	@echo "  Generated completions/ (bash, zsh, fish)"

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR) completions coverage.out coverage.html

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

## loadtest: Run load test (small — Synapses repo, ~2 min)
loadtest:
	go test -tags loadtest -run TestLoadProfile_Small -v -count 1 -p 1 -parallel 1 -timeout 600s ./internal/loadtest/

## loadtest/generate: Generate synthetic repos in /tmp for medium/large load tests
loadtest/generate:
	go run internal/loadtest/cmd/generate.go -files 10000 -out /tmp/synthetic_10k
	go run internal/loadtest/cmd/generate.go -files 50000 -out /tmp/synthetic_50k

## help: Print this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
