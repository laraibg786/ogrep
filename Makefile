SHELL := /bin/bash

BINARY  := ogrep
CMD     := ./cmd/ogrep
PKGS    := ./...
BIN_DIR := bin

# -buildvcs=false sidesteps "error obtaining VCS status" failures go
# build/test/vet, and tools like staticcheck that shell out to `go list`
# under the hood, otherwise hit when run from a git worktree or a
# checkout whose ownership doesn't match the running user (e.g. some
# CI/sandbox setups) — harmless here since release binaries get their
# version stamped explicitly via -ldflags (see the build/install targets
# and .goreleaser.yaml), not from VCS info embedded by the toolchain.
# Exported (not just a make variable) so it also reaches subprocesses
# spawned by tools like staticcheck, not only the go commands below that
# reference it directly.
export GOFLAGS := -buildvcs=false

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Platforms cross-compiled in CI's sanity check (.github/workflows/ci.yml);
# kept here too so `make cross` reproduces that check locally.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

## --- Build & run ---------------------------------------------------

.PHONY: build
build: ## Build the ogrep binary into bin/
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY) $(CMD)

.PHONY: run
run: ## Build and run ogrep, e.g. `make run ARGS='needle .'`
	go run $(CMD) $(ARGS)

.PHONY: install
install: ## Install ogrep into $GOBIN (or $GOPATH/bin)
	CGO_ENABLED=0 go install -ldflags "-X main.version=$(VERSION)" $(CMD)

.PHONY: cross
cross: ## Cross-compile for every release target platform (sanity check, matches CI)
	@mkdir -p $(BIN_DIR)/cross
	@for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		out=$(BIN_DIR)/cross/$(BINARY)-$$goos-$$goarch; \
		[ "$$goos" = "windows" ] && out="$$out.exe"; \
		echo "Building for $$goos/$$goarch..."; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -o "$$out" $(CMD) || exit 1; \
	done

## --- Formatting, vetting, static analysis ---------------------------

.PHONY: fmt
fmt: ## Reformat all Go source files in place
	gofmt -w -l .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source file isn't gofmt-formatted (matches CI)
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet $(PKGS)

.PHONY: staticcheck
staticcheck: ## Run staticcheck (fetched on demand, not vendored as a dependency)
	go run honnef.co/go/tools/cmd/staticcheck@latest $(PKGS)

.PHONY: lint
lint: fmt-check vet staticcheck ## Run all static checks: fmt-check + vet + staticcheck

## --- Testing ---------------------------------------------------------

.PHONY: test
test: ## Run the test suite
	go test $(PKGS)

.PHONY: test-race
test-race: ## Run the test suite with the race detector (matches CI)
	go test -race $(PKGS)

.PHONY: test-cover
test-cover: ## Run tests with coverage, writing bin/coverage.out and a summary
	@mkdir -p $(BIN_DIR)
	go test -coverprofile=$(BIN_DIR)/coverage.out $(PKGS)
	go tool cover -func=$(BIN_DIR)/coverage.out

.PHONY: test-cover-html
test-cover-html: test-cover ## Open an HTML coverage report
	go tool cover -html=$(BIN_DIR)/coverage.out

.PHONY: bench
bench: ## Run benchmarks (internal/core/app's perf_bench_test.go and any others)
	go test -run '^$$' -bench . -benchmem $(PKGS)

## --- Everything -------------------------------------------------------

.PHONY: check
check: fmt-check vet test-race ## What CI runs: fmt-check + vet + test -race (skips staticcheck/cross for speed)

.PHONY: ci
ci: fmt-check vet test-race cross ## Full CI-equivalent run, including the cross-compile sanity check

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
