package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// traceLog orders the mirror commits and lifecycle states the fixture observes.
// The session pump and the test both write to it.
type traceLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *traceLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = append(l.entries, entry)
}

func (l *traceLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.entries = nil
}

func (l *traceLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.entries...)
}

type nativeFixtureStore struct {
	acpcore.SessionStore
	trace *traceLog
	fail  atomic.Bool
}

func (s *nativeFixtureStore) Replace(ctx context.Context, key acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("fixture mirror failure")
	}
	if err := s.SessionStore.Replace(ctx, key, replacements); err != nil {
		return err
	}
	s.trace.add("commit")

	return nil
}

type nativeFixtureRecorder struct {
	*recorder
	trace *traceLog
}

func (r *nativeFixtureRecorder) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if envelope, ok := notification.Meta[wire.LifecycleKey].(map[string]any); ok {
		if event, ok := envelope["event"].(map[string]any); ok && event["type"] == "state_update" {
			state, _ := event["state"].(string)
			r.trace.add(state)
		}
	}
	if notification.Update.AgentMessageChunk != nil || notification.Update.UserMessageChunk != nil {
		r.trace.add("message")
	}

	return r.recorder.SessionUpdate(ctx, notification)
}

func TestCapturedNativeAgentOrigin(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		name := "committed"
		if failCommit {
			name = "failed commit"
		}
		t.Run(name, func(t *testing.T) {
			trace := &traceLog{}
			store := &nativeFixtureStore{SessionStore: acpcore.NewInMemorySessionStore(), trace: trace}
			rec := &nativeFixtureRecorder{recorder: newRecorder(), trace: trace}
			a := NewAgent(testOptions(t, WithSessionStore(store))...)
			t.Cleanup(func() { _ = a.Close() })
			a.attach(rec, nil)
			initResponse, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
			require.NoError(t, err)
			created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
			require.NoError(t, err)
			s, err := a.session(t.Context(), created.SessionId)
			require.NoError(t, err)
			s.mu.Lock()
			rt := s.runtime
			require.Nil(t, s.turn)
			s.mu.Unlock()
			data, err := os.ReadFile("testdata/native/agent-origin.json")
			require.NoError(t, err)
			data = []byte(strings.ReplaceAll(string(data), "fixture-session", string(s.id)))
			var frames []json.RawMessage
			require.NoError(t, json.Unmarshal(data, &frames))
			trace.reset()
			store.fail.Store(failCommit)
			for _, frame := range frames {
				var envelope struct {
					Payload opencode.Event `json:"payload"`
				}
				require.NoError(t, json.Unmarshal(frame, &envelope))
				rt.events <- envelope.Payload
			}
			require.Eventually(t, func() bool {
				if failCommit {
					select {
					case <-rt.done:
						return true
					default:
						return false
					}
				}
				s.mu.Lock()
				settled := s.cycle == nil
				s.mu.Unlock()
				entries := trace.snapshot()

				return settled && len(entries) > 0 && entries[len(entries)-1] == "idle"
			}, testTimeout, time.Millisecond)
			require.NotEmpty(t, trace.snapshot())
			require.Equal(t, "running", trace.snapshot()[0])
			for _, event := range lifecycleEvents(rec.snapshot()) {
				if event["type"] == "state_update" {
					require.Equal(t, "activity", event["cause"])
				}
				require.NotEqual(t, "prompt_accepted", event["type"])
			}
			require.Equal(t, "msg_fixture_0", s.nativeMessage("msg_fixture_0").ID)
			s.mu.Lock()
			require.Nil(t, s.cycle)
			s.mu.Unlock()
			require.NoError(t, lifecycle.CheckAttribution(negotiatedAnswer(t, initResponse), sessionFrames(t, rec.snapshot(), created.SessionId)))
			if !failCommit {
				entries := trace.snapshot()
				require.Equal(t, []string{"commit", "idle"}, entries[len(entries)-2:])

				return
			}

			require.NotContains(t, trace.snapshot(), "idle")
			require.False(t, s.lc.Active(), "a failed mirror commit fences the incarnation")

			// The fence is terminal, so the incarnation's binding ends with it.
			select {
			case <-rt.done:
			case <-time.After(testTimeout):
				t.Fatal("a failed agent-cycle commit left the binding in place")
			}

			store.fail.Store(false)

			next := wire.TextPromptRequest(s.id, "after the fence")
			next.Meta = promptMeta(1)

			_, err = a.Prompt(t.Context(), next)
			require.NoError(t, err)

			streams := lifecycleStreams(rec.snapshot())
			require.Len(t, streams, 2, "the next prompt opens a new incarnation on a fresh binding")
			require.NotEqual(t, streams[0], streams[1])
		})
	}
}

func TestAssistantTextIsAppendOnly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	_, err := h.prompt(session.SessionId, "STREAM", nil)
	require.NoError(t, err)
	require.Equal(t, "abcdef", agentText(h.rec.snapshot()),
		"a finalized text that extends the streamed prefix delivers only the suffix")
}

func TestToolInputArrivesAfterPending(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	a := NewAgent()
	t.Cleanup(func() { _ = a.Close() })
	a.attach(rec, nil)
	s := &session{agent: a, id: "tool-input-session"}
	state := cycleState{}
	part := opencode.NativePart{ID: "part", CallID: "call", Type: partTool, Tool: nativeToolBash}
	for _, data := range []string{
		`{"status":"pending","input":{}}`,
		`{"status":"running","input":{"command":"pwd"}}`,
		`{"status":"completed","input":{"command":"pwd"},"output":"/work"}`,
	} {
		part.State = json.RawMessage(data)
		require.NoError(t, s.projectPart(t.Context(), &state, part, roleAssistant))
	}

	var started int
	var inputs []any
	for _, notification := range rec.snapshot() {
		if notification.Update.ToolCall != nil {
			started++
		}
		if update := notification.Update.ToolCallUpdate; update != nil {
			if input, ok := update.RawInput.(map[string]any); ok && input["command"] != nil {
				inputs = append(inputs, input["command"])
			}
		}
	}
	require.Equal(t, 1, started)
	require.Equal(t, []any{"pwd", "pwd"}, inputs, "running and terminal updates preserve the command")
}

// fakeContextWindow is the fake catalog's context window for the session's
// model.
const fakeContextWindow = 32000

// TestUsageFollowsEachResponse proves every model call of a turn reports the
// context it left occupied, never the running sum, while the prompt response
// carries the turn's summed consumption.
func TestUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "MULTI", nil)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: fakeContextWindow, Used: 1120},
		{Size: fakeContextWindow, Used: 1200},
		{Size: fakeContextWindow, Used: 1250},
	}, usageUpdates(h.rec.snapshot()))
	require.NotNil(t, resp.Usage)
	require.Equal(t, 190, resp.Usage.InputTokens)
	require.Equal(t, 60, resp.Usage.OutputTokens)
	require.Equal(t, 3320, *resp.Usage.CachedReadTokens)
	require.Equal(t, 3570, resp.Usage.TotalTokens)
}

// TestUsageFollowsSteeredResponses proves the calls opencode makes for a
// message a native client added mid-turn report inside the turn and count
// toward its consumption, and that the added message's generation ends with
// the turn.
func TestUsageFollowsSteeredResponses(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(rec, nil)
	initResponse, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	request := wire.TextPromptRequest(created.SessionId, "STEER")
	request.Meta = promptMeta(1)
	resp, err := a.Prompt(t.Context(), request)
	require.NoError(t, err)

	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: fakeContextWindow, Used: 1120},
		{Size: fakeContextWindow, Used: 1200},
		{Size: fakeContextWindow, Used: 1250},
	}, usageUpdates(rec.snapshot()))
	require.Equal(t, 3570, resp.Usage.TotalTokens)
	require.NoError(t, lifecycle.CheckAttribution(negotiatedAnswer(t, initResponse), sessionFrames(t, rec.snapshot(), created.SessionId)))

	// opencode rewrites the added message once the run has ended; that late
	// frame opens no cycle of its own.
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.runtime
	s.mu.Unlock()
	messages, err := rt.client.Messages(t.Context(), s.cwd, s.nativeID)
	require.NoError(t, err)
	steer := slices.IndexFunc(messages, func(message opencode.NativeMessage) bool {
		return message.Info.Role == roleUser && message.Parts[0].Text == "also this"
	})
	require.GreaterOrEqual(t, steer, 0)
	late, err := json.Marshal(map[string]any{"info": messages[steer].Info})
	require.NoError(t, err)
	rt.events <- opencode.Event{Type: eventMessageUpdated, Properties: late}
	rt.events <- opencode.Event{Type: "todo.updated", Properties: json.RawMessage(`{"todos":[]}`)}
	rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		return slices.ContainsFunc(updates, func(update acp.SessionNotification) bool { return update.Update.Plan != nil })
	})
	s.mu.Lock()
	opened := s.cycle != nil
	s.mu.Unlock()
	require.False(t, opened)
}

// TestSteeredUsageArrivesInsideTheTurn proves a steered call reports as it
// finishes, while the turn is still running.
func TestSteeredUsageArrivesInsideTheTurn(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	done := make(chan acp.PromptResponse, 1)

	go func() {
		resp, _ := h.prompt(session.SessionId, "STEERSLOW", nil)
		done <- resp
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates)) == 2 })
	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: fakeContextWindow, Used: 1120},
		{Size: fakeContextWindow, Used: 1200},
	}, usageUpdates(h.rec.snapshot()))
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)
}

// TestUnusableResponsesReportNoUsage proves a failed attempt opencode retries
// inside the call reports nothing of its own.
func TestUnusableResponsesReportNoUsage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "FLAKY", nil)
	require.NoError(t, err)
	require.Equal(t, []acp.SessionUsageUpdate{{Size: fakeContextWindow, Used: 1120}}, usageUpdates(h.rec.snapshot()))
	require.Equal(t, 1120, resp.Usage.TotalTokens)
}

// TestUsageAfterCompaction proves the compaction summary's own call never
// stands in for the session's context: the next figure is the compacted one.
// The summary still counts toward the turn's consumption.
func TestUsageAfterCompaction(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	resp, err := h.prompt(session.SessionId, "COMPACT", nil)
	require.NoError(t, err)
	require.Equal(t, []acp.SessionUsageUpdate{
		{Size: fakeContextWindow, Used: 1120},
		{Size: fakeContextWindow, Used: 320},
	}, usageUpdates(h.rec.snapshot()))
	require.Equal(t, 1120+1320+320, resp.Usage.TotalTokens)
}

func TestCancelledTurnReportsNoUsageAfterCancel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	session := h.newSession()

	done := make(chan acp.PromptResponse, 1)

	go func() {
		resp, _ := h.prompt(session.SessionId, "STEPSLOW", nil)
		done <- resp
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(usageUpdates(updates)) == 1 })
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)

	require.Equal(t, []acp.SessionUsageUpdate{{Size: fakeContextWindow, Used: 1120}}, usageUpdates(h.rec.snapshot()),
		"the call that finished after the cancel reports nothing")
}

// TestAgentOriginUsageFollowsEachResponse replays captured native runs as
// agent-origin work: every call reports its own context, with the window the
// catalog knows for its model, and a compaction summary reports nothing.
func TestAgentOriginUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	catalog := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.WriteFile(catalog, []byte(`{"providers":[
		{"id":"fake","name":"Fake","models":{"vision":{"id":"vision","name":"Vision","limit":{"context":32000}}}},
		{"id":"openrouter","name":"OpenRouter","models":{"qwen/qwen3.8-flash":{"id":"qwen/qwen3.8-flash","name":"Qwen3.8 Flash","limit":{"context":1000000,"output":131072}}}},
		{"id":"omp","name":"Gateway","models":{"openrouter/qwen/qwen3.8-flash":{"id":"openrouter/qwen/qwen3.8-flash","name":"Qwen3.8 Flash","limit":{"context":0,"output":0}}}}
	],"default":{"fake":"vision"}}`), 0o600))

	for name, tc := range map[string]struct {
		fixture string
		want    []acp.SessionUsageUpdate
	}{
		"gateway without a context window, compacted": {"testdata/native/compaction.json", []acp.SessionUsageUpdate{
			{Used: 10561}, {Used: 10735}, {Used: 10938}, {Used: 11202}, {Used: 11363}, {Used: 11512}, {Used: 11556}, {Used: 11263},
		}},
		"provider with a context window, steered": {"testdata/native/steer.json", []acp.SessionUsageUpdate{
			{Size: 1000000, Used: 10703}, {Size: 1000000, Used: 10970}, {Size: 1000000, Used: 11183}, {Size: 1000000, Used: 11302},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := newRecorder()
			a := NewAgent(testOptions(t, WithEnv(map[string]string{fakeOpenCodeEnv: "1", "ACP_GO_OPENCODE_TEST_PROVIDERS": catalog}))...)
			t.Cleanup(func() { _ = a.Close() })
			a.attach(rec, nil)
			initResponse, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
			require.NoError(t, err)
			created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
			require.NoError(t, err)
			s, err := a.session(t.Context(), created.SessionId)
			require.NoError(t, err)
			s.mu.Lock()
			rt := s.runtime
			s.mu.Unlock()

			data, err := os.ReadFile(tc.fixture)
			require.NoError(t, err)

			var frames []struct {
				Payload opencode.Event `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(data, &frames))

			for _, frame := range frames {
				rt.events <- frame.Payload
			}

			require.Eventually(t, func() bool {
				s.mu.Lock()
				settled := s.cycle == nil
				s.mu.Unlock()

				return settled && len(usageUpdates(rec.snapshot())) == len(tc.want)
			}, testTimeout, time.Millisecond)
			require.Equal(t, tc.want, usageUpdates(rec.snapshot()))
			require.NoError(t, lifecycle.CheckAttribution(negotiatedAnswer(t, initResponse), sessionFrames(t, rec.snapshot(), created.SessionId)))
		})
	}
}

func TestContextTokens(t *testing.T) {
	t.Parallel()

	tokens := func(input, output, reasoning, read, write float64) opencode.NativeTokens {
		value := opencode.NativeTokens{Input: input, Output: output, Reasoning: reasoning}
		value.Cache.Read, value.Cache.Write = read, write

		return value
	}

	for name, tc := range map[string]struct {
		tokens opencode.NativeTokens
		want   int
		ok     bool
	}{
		"every component":              {tokens(1, 2, 3, 4, 5), 15, true},
		"cache reads, no cache writes": {tokens(3041, 72, 24, 7424, 0), 10561, true},
		"no cache reported":            {tokens(9524, 57, 17, 0, 0), 9598, true},
		"no usage":                     {opencode.NativeTokens{}, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			used, ok := contextTokens(tc.tokens)
			require.Equal(t, tc.want, used)
			require.Equal(t, tc.ok, ok)
		})
	}
}
