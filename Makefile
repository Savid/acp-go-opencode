.DEFAULT_GOAL := help

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test-cross-compile test-integration-attended test-integration-cover test-integration-keystore test-integration-live test-integration-native-browser test-integration-smoke tidy vuln

## build: build all packages
build:
	go build ./...

GO_TEST_TIMEOUT ?= 40m

## test: run unit tests with race detector and shuffled order
test:
	go test -race -shuffle=on -timeout=$(GO_TEST_TIMEOUT) ./...

## test-integration-native-browser: run current OpenCode login offline and trace every browser launcher
test-integration-native-browser:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ (set -eu; export ACP_GO_OPENCODE_RUN_INTEGRATION=1; case "$$(uname -m)" in x86_64) goarch=amd64; platform=linux/amd64 ;; arm64|aarch64) goarch=arm64; platform=linux/arm64 ;; *) echo "unsupported native-browser architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	integration/browser_canary/prepare.sh; \
	CGO_ENABLED=0 GOOS=linux GOARCH="$$goarch" go test -c -tags=integration,browsercanary -o .tmp/browser-canary/browser-canary.test .; \
	docker build --platform "$$platform" --tag acp-go-opencode-browser-canary --file integration/browser_canary/Dockerfile .; \
	docker run --rm --platform "$$platform" --network none --env ACP_GO_OPENCODE_RUN_INTEGRATION=1 --cap-add SYS_PTRACE --security-opt seccomp=unconfined acp-go-opencode-browser-canary); echo $$? >"$$rc"; } 2>&1 | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '^--- PASS: TestRealNativeBrowserLaunchIsNeutralized ' "$$log" || true); skipped=$$(grep -Ec '^[[:space:]]*--- SKIP: TestRealNativeBrowserLaunchIsNeutralized(/| )' "$$log" || true); empty=$$(grep -Ec 'no tests to run' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq 1 ] || { echo "native browser pass count $$passed, want exactly 1"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo 'required native browser canary skipped'; exit 1; }; \
	[ "$$empty" -eq 0 ] || { echo 'required native browser selector ran no tests'; exit 1; }

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
	go test -race -coverprofile=coverage.out -covermode=atomic -timeout=$(GO_TEST_TIMEOUT) ./...
	@awk 'NR > 1 && $$(NF - 1) > 0 && $$NF == 0 { print "uncovered statement block: " $$0; missed = 1 } END { if (missed) exit 1 }' coverage.out
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0 } /^total:/ { found = 1; if ($$3 != "100.0%") { printf "total coverage %s, want 100.0%%\n", $$3; exit 1 } printf "total coverage %s\n", $$3 } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=600s -v ./integration/... ./internal/opencode/...

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

## docs-audit: check required docs files and CLI flag docs
docs-audit:
	@missing=0; for file in README.md doc.go docs.json example_test.go AGENTS.md docs/overview.mdx docs/core/sessions.mdx docs/core/prompt-streaming.mdx docs/features/authentication.mdx docs/features/elicitation.mdx docs/features/mcp.mdx docs/features/models-config.mdx docs/features/permissions.mdx docs/features/raw-events.mdx docs/features/session-store.mdx docs/get-started/examples.mdx docs/get-started/install.mdx docs/get-started/quickstart.mdx docs/get-started/run-modes.mdx docs/operations/observability.mdx docs/operations/security.mdx docs/operations/troubleshooting.mdx docs/reference/acp-methods.mdx docs/reference/cli.mdx docs/reference/go-api.mdx docs/reference/meta.mdx docs/reference/updates.mdx examples/minimal-client/main.go examples/resume-from-file/main.go examples/interactive-chat/main.go; do if [ ! -f "$$file" ]; then echo "missing required docs file: $$file"; missing=1; fi; done; exit $$missing
	@for flag in -path -home -scratch-dir -provider-auth-root -provider-auth-direct-home -model -debug -version -seed-file -opencode-pure -opencode-question-tool -opencode-log-level -opencode-health-timeout; do rg -q -- "$$flag" docs/reference/cli.mdx cmd/acp-go-opencode/main.go || { echo "missing CLI flag in docs/code: $$flag"; exit 1; }; done

## audit: run repository checks
audit: fmt-check lint build test coverage-check test-cross-compile tidy vuln modernize-check docs-audit
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
