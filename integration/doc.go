// Package integration holds the tests that run against an installed OpenCode.
//
// The tests are behind the integration build tag and ACP_GO_OPENCODE_RUN_INTEGRATION=1.
// The smoke tier spends no model tokens; ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1 enables
// prompts that do.
//
// The harness binary comes from PATH; an absent binary skips. Live tests copy
// native credentials into a temporary home. ACP_GO_OPENCODE_MODEL selects the
// model for live tests.
package integration
