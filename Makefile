.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test-cross-compile test-integration-cover test-integration-live test-integration-smoke tidy vuln

REMOVED_PUBLIC_TERMS = opencode\x20acp|pro\x78y|compatibilit\x79|deprecat\x65d|legac\x79|migratio\x6e|session/imp\x6frt|sdkMessag\x65|emitRawSDKMessag\x65s|setGoa\x6c|goa\x6cs|\x4e\x45\x53|SSE\x20MCP|mcpCapabilities\x2eacp|ExportSessio\x6e|ImportSessio\x6e|DeleteSessio\x6e|ParseConfi\x67|OpenCodeSessio\x6e

## build: build all packages
build:
	go build ./...

## test: run unit tests with race detector and shuffled order
test:
	go test -race -shuffle=on ./...

## test-cross-compile: compile-check platform branches for other GOOS targets
test-cross-compile:
	rm -rf .tmp/cross
	mkdir -p .tmp/cross
	GOOS=linux GOARCH=amd64 go test -c -o .tmp/cross/opencode-linux.test .
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/opencode-darwin.test .
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/opencode-windows.test .
	GOOS=freebsd GOARCH=amd64 go build ./...
	GOOS=openbsd GOARCH=amd64 go build ./...
	GOOS=windows GOARCH=amd64 go build ./...

## coverage-check: require 100% statement coverage with race instrumentation
coverage-check:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0 } /^total:/ { found = 1; if ($$3 != "100.0%") { printf "total coverage %s, want 100.0%%\n", $$3; exit 1 } printf "total coverage %s\n", $$3 } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=240s -v ./integration/...

## test-integration-live: run live integration tests that spend model tokens
test-integration-live:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=180s -v ./integration/...

## test-integration-cover: run smoke integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-opencode ./cmd/acp-go-opencode
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-opencode GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -count=1 -tags=integration -timeout=240s -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## lint: run pinned golangci-lint
lint:
	$(GOLANGCI_LINT) run ./...

## fmt-check: require gofmt-clean Go files
fmt-check:
	@test -z "$$(gofmt -l .)"

## fmt: format Go files
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
	$(GOLANGCI_LINT) fmt ./...

## tidy: verify module files are tidy
tidy:
	go mod tidy -diff

## vuln: run govulncheck from the go.mod tool directive
vuln:
	go tool govulncheck ./...

## modernize-check: check Go modernizations without changing files
modernize-check:
	go fix -n ./...

## docs-audit: check public docs and examples for removed public terms
docs-audit:
	@pattern=$$(printf '%b' '$(REMOVED_PUBLIC_TERMS)'); ! rg -n -- "$$pattern" README.md doc.go docs.json docs examples cmd/acp-go-opencode/*.go AGENTS.md

## audit: run repository checks
audit: fmt-check lint build test coverage-check test-cross-compile tidy vuln modernize-check docs-audit
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
