.DEFAULT_GOAL := help
.PHONY: audit build clean coverage-check fmt help lint modernize-check test test/cover test-integration test-integration-cover test-integration-live test-integration-smoke tidy vuln

## build: build all packages
build:
	go build ./...

## test: run tests with race detector and coverage
test:
	go test -race -shuffle=on -coverprofile=coverage.out -covermode=atomic ./...

## coverage-check: run tests and print total statement coverage
coverage-check: test
	@go tool cover -func=coverage.out | awk 'BEGIN { found = 0; min = 100.0 } /^total:/ { found = 1; coverage = $$3; sub(/%/, "", coverage); if (coverage + 0 < min) { printf "total coverage %s, want at least %.1f%%\n", $$3, min; exit 1 } printf "total coverage %s\n", $$3 } END { if (!found) { print "missing total coverage line"; exit 1 } }'

## test-integration-smoke: run live integration tests that do not spend model tokens
test-integration-smoke:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 go test -race -count=1 -tags=integration -timeout=120s -v ./integration/...

## test-integration-live: run live integration tests that spend model tokens
test-integration-live:
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1 go test -race -count=1 -tags=integration -timeout=180s -v ./integration/...

## test-integration: run live integration smoke tests
test-integration: test-integration-smoke

## test-integration-cover: run smoke integration tests with compiled binary coverage
test-integration-cover:
	rm -rf .tmp/integration-cover coverage-integration.out
	mkdir -p .tmp/integration-cover/data
	go build -cover -coverpkg=./... -o .tmp/integration-cover/acp-go-opencode ./cmd/acp-go-opencode
	ACP_GO_OPENCODE_RUN_INTEGRATION=1 ACP_GO_OPENCODE_AGENT_BINARY=$$(pwd)/.tmp/integration-cover/acp-go-opencode GOCOVERDIR=$$(pwd)/.tmp/integration-cover/data go test -race -count=1 -tags=integration -timeout=120s -v ./integration/...
	go tool covdata percent -i=.tmp/integration-cover/data
	go tool covdata textfmt -i=.tmp/integration-cover/data -o coverage-integration.out

## lint: run golangci-lint
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...

## fmt: format code with golangci-lint
fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 fmt ./...

## tidy: tidy go modules
tidy:
	go mod tidy

## vuln: run govulncheck
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## modernize-check: preview Go modernizations without changing files
modernize-check:
	go fix -n ./...

## audit: run local checks
audit: lint build coverage-check
	go mod tidy -diff
	@if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then git diff --exit-code -- go.mod go.sum; fi
	go mod verify

## clean: remove build artifacts
clean:
	rm -rf .tmp coverage.out coverage-integration.out coverage-summary.txt

## test/cover: open HTML coverage report
test/cover: test
	go tool cover -html=coverage.out

## help: show this help
help:
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
