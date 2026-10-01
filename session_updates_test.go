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
		callReport(fakeContextWindow, 1120, 100, 1000, 0, 20),
		callReport(fakeContextWindow, 1200, 50, 1120, 0, 30),
		callReport(fakeContextWindow, 1250, 40, 1200, 0, 10),
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
		callReport(fakeContextWindow, 1120, 100, 1000, 0, 20),
		callReport(fakeContextWindow, 1200, 50, 1120, 0, 30),
		callReport(fakeContextWindow, 1250, 40, 1200, 0, 10),
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
		callReport(fakeContextWindow, 1120, 100, 1000, 0, 20),
		callReport(fakeContextWindow, 1200, 50, 1120, 0, 30),
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
	require.Equal(t, []acp.SessionUsageUpdate{callReport(fakeContextWindow, 1120, 100, 1000, 0, 20)}, usageUpdates(h.rec.snapshot()))
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
		callReport(fakeContextWindow, 1120, 100, 1000, 0, 20),
		callReport(fakeContextWindow, 320, 300, 0, 0, 20),
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

	require.Equal(t, []acp.SessionUsageUpdate{callReport(fakeContextWindow, 1120, 100, 1000, 0, 20)}, usageUpdates(h.rec.snapshot()),
		"the call that finished after the cancel reports nothing")
}

// nativeCatalog is a provider catalog holding the models the captured native
// runs used.
func nativeCatalog(t *testing.T) string {
	t.Helper()

	catalog := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.WriteFile(catalog, []byte(`{"providers":[
		{"id":"fake","name":"Fake","models":{"vision":{"id":"vision","name":"Vision","limit":{"context":32000}}}},
		{"id":"openrouter","name":"OpenRouter","models":{"qwen/qwen3.8-flash":{"id":"qwen/qwen3.8-flash","name":"Qwen3.8 Flash","limit":{"context":1000000,"output":131072}}}},
		{"id":"omp","name":"Gateway","models":{"openrouter/qwen/qwen3.8-flash":{"id":"openrouter/qwen/qwen3.8-flash","name":"Qwen3.8 Flash","limit":{"context":0,"output":0}}}}
	],"default":{"fake":"vision"}}`), 0o600))

	return catalog
}

// replayNativeRun feeds a captured native run to a fresh session as
// agent-origin work and returns its updates once the cycle settles with
// usageReports usage updates delivered.
func replayNativeRun(t *testing.T, catalog, fixture string, usageReports int) (acp.SessionId, []acp.SessionNotification) {
	t.Helper()

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

	data, err := os.ReadFile(fixture)
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

		return settled && len(usageUpdates(rec.snapshot())) == usageReports
	}, testTimeout, time.Millisecond)

	updates := rec.snapshot()
	require.NoError(t, lifecycle.CheckAttribution(negotiatedAnswer(t, initResponse), sessionFrames(t, updates, created.SessionId)))

	return created.SessionId, updates
}

// TestAgentOriginUsageFollowsEachResponse replays captured native runs as
// agent-origin work: every call reports its own context, with the window the
// catalog knows for its model, and a compaction summary reports nothing.
func TestAgentOriginUsageFollowsEachResponse(t *testing.T) {
	t.Parallel()

	catalog := nativeCatalog(t)

	for name, tc := range map[string]struct {
		fixture string
		want    []acp.SessionUsageUpdate
	}{
		"gateway without a context window, compacted": {"testdata/native/compaction.json", []acp.SessionUsageUpdate{
			callReport(0, 10561, 3041, 7424, 0, 96),
			callReport(0, 10735, 383, 10240, 0, 112),
			callReport(0, 10938, 301, 10496, 0, 141),
			callReport(0, 11202, 248, 10752, 0, 202),
			callReport(0, 11363, 530, 10752, 0, 81),
			callReport(0, 11512, 165, 11264, 0, 83),
			callReport(0, 11556, 274, 11264, 0, 18),
			callReport(0, 11263, 686, 10240, 0, 337),
		}},
		"provider with a context window, steered": {"testdata/native/steer.json", []acp.SessionUsageUpdate{
			callReport(1000000, 10703, 3130, 7424, 0, 149),
			callReport(1000000, 10970, 248, 10496, 0, 226),
			callReport(1000000, 11183, 492, 10496, 0, 195),
			callReport(1000000, 11302, 465, 10752, 0, 85),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, updates := replayNativeRun(t, catalog, tc.fixture, len(tc.want))
			require.Equal(t, tc.want, usageUpdates(updates))
		})
	}
}

// responseChunk is one agent message or thought chunk as a client sees it.
type responseChunk struct {
	thought   bool
	text      string
	messageID *string
}

// responseChunks returns the agent message and thought chunks of one session
// in delivery order.
func responseChunks(updates []acp.SessionNotification, id acp.SessionId) []responseChunk {
	var chunks []responseChunk

	for _, update := range updates {
		if update.SessionId != id {
			continue
		}

		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			chunks = append(chunks, responseChunk{text: chunk.Content.Text.Text, messageID: chunk.MessageId})
		}

		if chunk := update.Update.AgentThoughtChunk; chunk != nil && chunk.Content.Text != nil {
			chunks = append(chunks, responseChunk{thought: true, text: chunk.Content.Text.Text, messageID: chunk.MessageId})
		}
	}

	return chunks
}

// TestResponsesCarryNoGatewayID proves the adapter attributes no response id.
// opencode keeps none of the gateway's response ids: its message and part ids
// are its own, so chunks carry no message id and call usage no response id.
// The captured run is one OpenRouter call that streamed its reasoning and
// text; the gateway answered it with a generation id opencode never surfaced.
func TestResponsesCarryNoGatewayID(t *testing.T) {
	t.Parallel()

	t.Run("streamed", func(t *testing.T) {
		t.Parallel()

		id, updates := replayNativeRun(t, nativeCatalog(t), "testdata/native/response.json", 1)
		require.Equal(t, []responseChunk{
			{thought: true, text: "The"},
			{thought: true, text: " user wants me to"},
			{thought: true, text: " reply with just the"},
			{thought: true, text: ` word "hi".`},
			{text: "hi"},
		}, responseChunks(updates, id))

		report := usageUpdates(updates)
		require.Equal(t, []acp.SessionUsageUpdate{callReport(1000000, 4914, 3105, 1792, 0, 17)}, report)

		call, ok := report[0].Meta[wire.CallUsageKey].(map[string]any)
		require.True(t, ok)
		require.NotContains(t, call, "responseId")
	})

	t.Run("replayed", func(t *testing.T) {
		t.Parallel()

		store := acpcore.NewInMemorySessionStore()
		h := newHarness(t, WithSessionStore(store))
		h.initialize()
		cwd := t.TempDir()
		created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
		require.NoError(t, err)
		_, err = h.prompt(created.SessionId, "HELLO", nil)
		require.NoError(t, err)
		_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
		require.NoError(t, err)

		before := len(h.rec.snapshot())
		_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
		require.NoError(t, err)

		replayed := responseChunks(h.rec.snapshot()[before:], created.SessionId)
		require.NotEmpty(t, replayed)
		for _, chunk := range replayed {
			require.Nil(t, chunk.messageID)
		}
	})
}

// TestCallTokens proves one call's native tokens give the context it left
// occupied and its breakdown: opencode's input is already uncached, and its
// reasoning joins the output. A call without any token is unknown.
func TestCallTokens(t *testing.T) {
	t.Parallel()

	tokens := func(input, output, reasoning, read, write float64) opencode.NativeTokens {
		value := opencode.NativeTokens{Input: input, Output: output, Reasoning: reasoning}
		value.Cache.Read, value.Cache.Write = read, write

		return value
	}

	for name, tc := range map[string]struct {
		tokens                     opencode.NativeTokens
		used                       int
		input, read, write, output int
		known                      bool
	}{
		"every component":              {tokens(1, 2, 3, 4, 5), 15, 1, 4, 5, 5, true},
		"cache reads, no cache writes": {tokens(3041, 72, 24, 7424, 0), 10561, 3041, 7424, 0, 96, true},
		"no cache reported":            {tokens(9524, 57, 17, 0, 0), 9598, 9524, 0, 0, 74, true},
		"reasoning only":               {tokens(0, 0, 7, 0, 0), 7, 0, 0, 0, 7, true},
		"replayed from a cache":        {opencode.NativeTokens{}, 0, 0, 0, 0, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.used, contextTokens(tc.tokens))

			call := callUsage(tc.tokens)
			require.Equal(t, wire.CallUsage{InputTokens: &tc.input, CachedReadTokens: &tc.read, CachedWriteTokens: &tc.write, OutputTokens: &tc.output}, call)
			require.Equal(t, tc.known, call.Known())
		})
	}
}
