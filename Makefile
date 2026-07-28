.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test-cross-compile test-integration-attended test-integration-cover test-integration-keystore test-integration-live test-integration-smoke tidy vuln

REMOVED_PUBLIC_TERMS = opencode\x20acp|pro\x78y|compatibilit\x79|deprecat\x65d|legac\x79|migratio\x6e|session/imp\x6frt|sdkMessag\x65|emitRawSDKMessag\x65s|setGoa\x6c|goa\x6cs|\x4e\x45\x53|SSE\x20MCP|mcpCapabilities\x2eacp|ExportSessio\x6e|ImportSessio\x6e|DeleteSessio\x6e|ParseConfi\x67|OpenCodeSessio\x6e|(^|[^.])WithVersio\x6e|opencode-versio\x6e

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
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/opencode-internal-darwin.test ./internal/opencode
	GOOS=darwin GOARCH=arm64 go test -c -o .tmp/cross/opencode-cmd-darwin.test ./cmd/acp-go-opencode
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/opencode-windows.test .
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/opencode-internal-windows.test ./internal/opencode
	GOOS=windows GOARCH=amd64 go test -c -o .tmp/cross/opencode-cmd-windows.test ./cmd/acp-go-opencode
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

## test-integration-attended: run provider-auth flows a human must approve in real time
test-integration-attended:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_RUN_ATTENDED=1 go test -race -count=1 -tags=integration -timeout=1200s -v -run TestAttended ./integration/... 2>&1; echo $$? >"$$rc"; } | tee "$$log"; \
	status=$$(cat "$$rc"); ran=$$(grep -c '^--- PASS: TestAttended' "$$log"); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$ran" -gt 0 ] || { echo 'no attended provider-auth login ran: -run TestAttended selected nothing'; exit 1; }

## test-integration-keystore: run credential-residence tests against the container fixture
test-integration-keystore:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_RUN_KEYSTORE=1 go test -race -count=1 -tags=integration -timeout=600s -v -run TestKeystore ./...

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

## docs-audit: check required docs files, CLI flag docs, and removed public terms
docs-audit:
	@missing=0; for file in README.md doc.go docs.json example_test.go AGENTS.md docs/overview.mdx docs/core/sessions.mdx docs/core/prompt-streaming.mdx docs/features/authentication.mdx docs/features/elicitation.mdx docs/features/mcp.mdx docs/features/models-config.mdx docs/features/permissions.mdx docs/features/raw-events.mdx docs/features/session-store.mdx docs/get-started/examples.mdx docs/get-started/install.mdx docs/get-started/quickstart.mdx docs/get-started/run-modes.mdx docs/operations/observability.mdx docs/operations/security.mdx docs/operations/troubleshooting.mdx docs/reference/acp-methods.mdx docs/reference/cli.mdx docs/reference/go-api.mdx docs/reference/meta.mdx docs/reference/updates.mdx examples/minimal-client/main.go examples/resume-from-file/main.go examples/interactive-chat/main.go; do if [ ! -f "$$file" ]; then echo "missing required docs file: $$file"; missing=1; fi; done; exit $$missing
	@for flag in -path -home -scratch-dir -provider-auth-root -provider-auth-direct-home -darwin-best-effort-containment -model -debug -version -seed-file -opencode-pure -opencode-question-tool -opencode-log-level -opencode-health-timeout; do rg -q -- "$$flag" docs/reference/cli.mdx cmd/acp-go-opencode/main.go || { echo "missing CLI flag in docs/code: $$flag"; exit 1; }; done
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
