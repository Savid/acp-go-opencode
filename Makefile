.DEFAULT_GOAL := help

.PHONY: test-trusted-supervisor _privileged-shard-guard _privileged-shard-coverage _privileged-shard-trusted-supervisor _privileged-coverage-gate

GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: audit build clean coverage-check docs-audit fmt fmt-check help lint modernize-check test test-cross-compile test-integration-attended test-integration-cover test-integration-keystore test-integration-live test-integration-native-browser test-integration-smoke tidy vuln

REMOVED_PUBLIC_TERMS = opencode\x20acp|pro\x78y|compatibilit\x79|deprecat\x65d|legac\x79|migratio\x6e|session/imp\x6frt|sdkMessag\x65|emitRawSDKMessag\x65s|setGoa\x6c|goa\x6cs|\x4e\x45\x53|SSE\x20MCP|mcpCapabilities\x2eacp|ExportSessio\x6e|ImportSessio\x6e|DeleteSessio\x6e|ParseConfi\x67|OpenCodeSessio\x6e|(^|[^.])WithVersio\x6e|opencode-versio\x6e|SessionStartupFailur\x65|opencode_session_start_faile\x64|RuntimeStartupCarrie\x72

## build: build all packages
build:
	go build ./...

# Every isolated native launch claims the standalone agent identity, and that
# claim proves the identity vacant across every task in the PID namespace. In
# the initial namespace the suite requires, that is the whole host, so the
# adapter package runs far past the ten-minute default.
GO_TEST_TIMEOUT ?= 40m

## test: run unit tests with race detector and shuffled order
test:
	go test -race -shuffle=on -timeout=$(GO_TEST_TIMEOUT) ./...

## test-trusted-supervisor: run Linux root-only native authority tests
test-trusted-supervisor:
	@test "$$(uname -s)" = Linux
	@test "$$(id -u)" -eq 0
	@for directory in /var/lib/acp-go /var/lib/acp-go/agent-identities; do if [ ! -e "$$directory" ] && [ ! -L "$$directory" ]; then install -d -o root -g root -m 0700 "$$directory"; fi; [ "$$(stat -c '%F %u %g %a' -- "$$directory")" = 'directory 0 0 700' ] || { echo "unsafe trusted-supervisor authority directory $$directory" >&2; exit 1; }; done
	@selector='^(Test.*(ProcessIsolationActual|TrustedSupervisor|SupervisorGuardianSIGKILL|SupervisorLivenessSIGKILL|GeneratedNative|NativeOwnedDirectory|BorrowedIdentityAdoption|BorrowedDomainAdoption|BorrowedDisposition|AgentIdentityLock|AgentStandalone|AuthorityDomain|IdentityDisposition|PersistentProof|SupervisorConfigIsSealed|CommandCreatorThread|ProviderCreator|SecurityLimits).*)$$'; listing=$$(mktemp); log=$$(mktemp); rc=$$(mktemp); module=$$(go list -mod=readonly -m); status=$$?; \
	[ "$$status" -eq 0 ] || { rm -f "$$listing" "$$log" "$$rc"; exit "$$status"; }; \
	go test -list "$$selector" ./... >"$$listing"; status=$$?; \
	[ "$$status" -eq 0 ] || { rm -f "$$listing" "$$log" "$$rc"; exit "$$status"; }; \
	required='TrustedSupervisor SupervisorGuardianSIGKILL SupervisorGuardianSIGKILLBeforeNativeLaunchRefusesStartAndCompletesAfterECHILD SupervisorLivenessSIGKILL GeneratedNative BorrowedIdentityAdoption BorrowedDomainAdoption BorrowedDisposition AgentIdentityLock AgentStandalone AuthorityDomain IdentityDisposition CommandCreatorThread SecurityLimits ProcessIsolationActual'; case "$$module" in github.com/savid/acp-go-amp|github.com/savid/acp-go-claude|github.com/savid/acp-go-hermes|github.com/savid/acp-go-pi) ;; github.com/savid/acp-go-codex|github.com/savid/acp-go-opencode) required="$$required PersistentProof SupervisorConfigIsSealed ProviderCreator" ;; *) rm -f "$$listing" "$$log" "$$rc"; echo "unrecognized trusted-supervisor module $$module"; exit 1 ;; esac; \
	for class in $$required; do grep -Eq "^Test.*$${class}" "$$listing" || { rm -f "$$listing" "$$log" "$$rc"; echo "trusted-supervisor selector discovered no $${class} tests"; exit 1; }; done; \
	expected=$$(grep -Ec '^Test' "$$listing" || true); rm -f "$$listing"; \
	[ "$$expected" -gt 0 ] || { rm -f "$$log" "$$rc"; echo 'trusted-supervisor selector discovered no tests'; exit 1; }; \
	{ go test -race -count=1 -json -run "$$selector" ./...; echo $$? >"$$rc"; } | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '"Action":"pass","Package":"[^"]+","Test":"Test[^/"]+"' "$$log" || true); skipped=$$(grep -Ec '"Action":"skip","Package":"[^"]+","Test":"Test[^"]+"' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq "$$expected" ] || { echo "trusted-supervisor pass count $$passed, want $$expected"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo "trusted-supervisor skip count $$skipped, want 0"; exit 1; }

## test-integration-native-browser: run current OpenCode login offline and trace every browser launcher
test-integration-native-browser:
	@log=$$(mktemp); rc=$$(mktemp); \
	{ (set -eu; export ACP_GO_OPENCODE_RUN_INTEGRATION=1; case "$$(uname -m)" in x86_64) goarch=amd64; platform=linux/amd64 ;; arm64|aarch64) goarch=arm64; platform=linux/arm64 ;; *) echo "unsupported native-browser architecture: $$(uname -m)" >&2; exit 1 ;; esac; \
	integration/browser_canary/prepare.sh; \
	CGO_ENABLED=0 GOOS=linux GOARCH="$$goarch" go test -c -tags=integration,browsercanary -o .tmp/browser-canary/browser-canary.test .; \
	docker build --platform "$$platform" --tag acp-go-opencode-browser-canary --file integration/browser_canary/Dockerfile .; \
	authority_volume=$$(docker volume create); trap 'docker volume rm "$$authority_volume" >/dev/null' EXIT HUP INT TERM; \
	docker run --rm --platform "$$platform" --network none --pid=host --env ACP_GO_OPENCODE_RUN_INTEGRATION=1 --cap-add SYS_PTRACE --security-opt seccomp=unconfined --tmpfs /tmp:rw,exec,size=256m --tmpfs /home/canary:rw,exec,uid=4242,gid=4242,mode=0700,size=128m --tmpfs /canary/scratch:rw,exec,mode=0711,size=128m --mount "type=volume,source=$$authority_volume,target=/var/lib/acp-go/agent-identities" acp-go-opencode-browser-canary); echo $$? >"$$rc"; } 2>&1 | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '^--- PASS: TestRealNativeBrowserContainment ' "$$log" || true); skipped=$$(grep -Ec '^[[:space:]]*--- SKIP: TestRealNativeBrowserContainment(/| )' "$$log" || true); empty=$$(grep -Ec 'no tests to run' "$$log" || true); \
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

# Private container-only shard verbs. Public release targets above always cover
# ./... and enforce their complete gates; only the privileged coordinator calls
# these explicitly partial targets after validating a six-module package map.
_privileged-shard-guard:
	@test '$(ACP_GO_PRIVILEGED_INTERNAL)' = 1
	@case '$(ACP_GO_PRIVILEGED_SHARD)' in root|provider) ;; *) echo 'invalid privileged shard $(ACP_GO_PRIVILEGED_SHARD)' >&2; exit 1 ;; esac
	@test -n '$(ACP_GO_PRIVILEGED_MODULE)'
	@test -n '$(ACP_GO_PRIVILEGED_PACKAGES)'
	@test -n '$(ACP_GO_PRIVILEGED_REQUIRED_CLASSES)'
	@test "$$(go list -mod=readonly -m)" = '$(ACP_GO_PRIVILEGED_MODULE)'

_privileged-shard-coverage: _privileged-shard-guard
	@case '$(ACP_GO_PRIVILEGED_COVERAGE_OUT)' in .tmp/coverage-*.out) ;; *) echo 'invalid privileged coverage output $(ACP_GO_PRIVILEGED_COVERAGE_OUT)' >&2; exit 1 ;; esac
	go test -race -coverprofile='$(ACP_GO_PRIVILEGED_COVERAGE_OUT)' -covermode=atomic -timeout=$(GO_TEST_TIMEOUT) $(ACP_GO_PRIVILEGED_PACKAGES)

_privileged-shard-trusted-supervisor: _privileged-shard-guard
	@test "$$(uname -s)" = Linux
	@test "$$(id -u)" -eq 0
	@for directory in /var/lib/acp-go /var/lib/acp-go/agent-identities; do if [ ! -e "$$directory" ] && [ ! -L "$$directory" ]; then install -d -o root -g root -m 0700 "$$directory"; fi; [ "$$(stat -c '%F %u %g %a' -- "$$directory")" = 'directory 0 0 700' ] || { echo "unsafe trusted-supervisor authority directory $$directory" >&2; exit 1; }; done
	@selector='^(Test.*(ProcessIsolationActual|TrustedSupervisor|SupervisorGuardianSIGKILL|SupervisorLivenessSIGKILL|GeneratedNative|NativeOwnedDirectory|BorrowedIdentityAdoption|BorrowedDomainAdoption|BorrowedDisposition|AgentIdentityLock|AgentStandalone|AuthorityDomain|IdentityDisposition|PersistentProof|SupervisorConfigIsSealed|CommandCreatorThread|ProviderCreator|SecurityLimits).*)$$'; listing=$$(mktemp); log=$$(mktemp); rc=$$(mktemp); \
	go test -list "$$selector" $(ACP_GO_PRIVILEGED_PACKAGES) >"$$listing"; status=$$?; \
	[ "$$status" -eq 0 ] || { rm -f "$$listing" "$$log" "$$rc"; exit "$$status"; }; \
	required='$(ACP_GO_PRIVILEGED_REQUIRED_CLASSES)'; \
	for class in $$required; do grep -Eq "^Test.*$${class}" "$$listing" || { rm -f "$$listing" "$$log" "$$rc"; echo "trusted-supervisor selector discovered no $${class} tests in $(ACP_GO_PRIVILEGED_SHARD) shard"; exit 1; }; done; \
	expected=$$(grep -Ec '^Test' "$$listing" || true); rm -f "$$listing"; \
	[ "$$expected" -gt 0 ] || { rm -f "$$log" "$$rc"; echo 'trusted-supervisor shard selector discovered no tests'; exit 1; }; \
	{ go test -race -count=1 -json -run "$$selector" $(ACP_GO_PRIVILEGED_PACKAGES); echo $$? >"$$rc"; } | tee "$$log"; \
	status=$$(cat "$$rc"); passed=$$(grep -Ec '"Action":"pass","Package":"[^"]+","Test":"Test[^/"]+"' "$$log" || true); skipped=$$(grep -Ec '"Action":"skip","Package":"[^"]+","Test":"Test[^"]+"' "$$log" || true); \
	rm -f "$$log" "$$rc"; \
	[ "$$status" -eq 0 ] || exit "$$status"; \
	[ "$$passed" -eq "$$expected" ] || { echo "trusted-supervisor pass count $$passed, want $$expected"; exit 1; }; \
	[ "$$skipped" -eq 0 ] || { echo "trusted-supervisor skip count $$skipped, want 0"; exit 1; }

_privileged-coverage-gate:
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

## docs-audit: check required docs files, CLI flag docs, and removed public terms
docs-audit:
	@missing=0; for file in README.md doc.go docs.json example_test.go AGENTS.md docs/overview.mdx docs/core/sessions.mdx docs/core/prompt-streaming.mdx docs/features/authentication.mdx docs/features/elicitation.mdx docs/features/mcp.mdx docs/features/models-config.mdx docs/features/permissions.mdx docs/features/raw-events.mdx docs/features/session-store.mdx docs/get-started/examples.mdx docs/get-started/install.mdx docs/get-started/quickstart.mdx docs/get-started/run-modes.mdx docs/operations/observability.mdx docs/operations/security.mdx docs/operations/troubleshooting.mdx docs/reference/acp-methods.mdx docs/reference/cli.mdx docs/reference/go-api.mdx docs/reference/meta.mdx docs/reference/updates.mdx examples/minimal-client/main.go examples/resume-from-file/main.go examples/interactive-chat/main.go; do if [ ! -f "$$file" ]; then echo "missing required docs file: $$file"; missing=1; fi; done; exit $$missing
	@for flag in -path -home -scratch-dir -provider-auth-root -provider-auth-direct-home -model -debug -version -seed-file -opencode-pure -opencode-question-tool -opencode-log-level -opencode-health-timeout; do rg -q -- "$$flag" docs/reference/cli.mdx cmd/acp-go-opencode/main.go || { echo "missing CLI flag in docs/code: $$flag"; exit 1; }; done
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
