package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
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
			_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber, Meta: map[string]any{wire.LifecycleKey: map[string]any{"version": 1}}})
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
				s.handleEvent(t.Context(), rt, envelope.Payload)
			}
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
