package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func compactionReports(t *testing.T, notifications []acp.SessionNotification) []wire.Compaction {
	t.Helper()
	var reports []wire.Compaction
	for _, notification := range notifications {
		value, exists := notification.Meta[wire.CompactionKey]
		if !exists {
			continue
		}
		carrier, err := json.Marshal(notification.Update)
		require.NoError(t, err)
		require.JSONEq(t, `{"sessionUpdate":"session_info_update"}`, string(carrier))
		require.Len(t, notification.Meta, 1)
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		var report wire.Compaction
		require.NoError(t, json.Unmarshal(encoded, &report))
		require.NotEmpty(t, report.CompactionID)
		reports = append(reports, report)
	}

	return reports
}

func TestCompactionTransport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize()
	created := h.newSession()
	for range 2 {
		_, err := h.prompt(created.SessionId, "COMPACT", nil)
		require.NoError(t, err)
	}
	reports := compactionReports(t, h.rec.snapshot())
	require.Len(t, reports, 4)
	require.NotEqual(t, reports[0].CompactionID, reports[2].CompactionID)
	for i := 0; i < len(reports); i += 2 {
		require.Equal(t, wire.CompactionCompleted, reports[i+1].Status)
		require.Equal(t, wire.CompactionInProgress, reports[i].Status)
		require.Equal(t, reports[i].CompactionID, reports[i+1].CompactionID)
		require.Nil(t, reports[i+1].ContextAfter)
		require.Equal(t, "auto", reports[i].Trigger)
		require.Equal(t, "auto", reports[i+1].Trigger)
	}
}

func TestCompactionSummaryOutcomes(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	for _, test := range []struct{ id, failure, status string }{
		{"failed", "APIError", wire.CompactionFailed},
		{"cancelled", "MessageAbortedError", wire.CompactionCancelled},
		{"success", "", wire.CompactionCompleted},
	} {
		info := opencode.NativeMessageInfo{ID: test.id, SessionID: "root", Role: roleAssistant, Summary: json.RawMessage(`true`)}
		event := opencode.Event{Type: eventMessageUpdated, Properties: json.RawMessage(`{"sessionID":"root"}`)}
		require.NoError(t, s.projectCompaction(t.Context(), event, eventProperties{Info: info}))
		require.NoError(t, s.projectCompaction(t.Context(), event, eventProperties{Info: info}))
		if test.failure != "" {
			info.Error = &opencode.NativeError{Name: test.failure}
		} else {
			event.Type = "session.compacted"
			event.ID = test.id + "-done"
		}
		require.NoError(t, s.projectCompaction(t.Context(), event, eventProperties{Info: info}))
		require.NoError(t, s.projectCompaction(t.Context(), event, eventProperties{Info: info}))
		reports := compactionReports(t, rec.snapshot())
		require.Equal(t, test.status, reports[len(reports)-1].Status)
		require.Empty(t, reports[len(reports)-1].Trigger)
		require.Equal(t, reports[len(reports)-2].CompactionID, reports[len(reports)-1].CompactionID)
	}
	event := opencode.Event{Type: "session.compacted", Properties: json.RawMessage(`{"sessionID":"child"}`)}
	require.NoError(t, s.projectCompaction(t.Context(), event, eventProperties{}))
	require.Len(t, compactionReports(t, rec.snapshot()), 6)
}

func TestCapturedCompactionOrdering(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := newRecorder()
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "fixture-session"
	raw, err := os.ReadFile("testdata/native/compaction.json")
	require.NoError(t, err)
	var frames []struct {
		Payload opencode.Event `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(raw, &frames))
	for _, frame := range frames {
		var properties eventProperties
		require.NoError(t, json.Unmarshal(frame.Payload.Properties, &properties))
		require.NoError(t, s.projectCompaction(t.Context(), frame.Payload, properties))
	}
	reports := compactionReports(t, rec.snapshot())
	require.Len(t, reports, 2)
	require.Equal(t, wire.CompactionInProgress, reports[0].Status)
	require.Equal(t, wire.CompactionCompleted, reports[1].Status)
	require.Equal(t, reports[0].CompactionID, reports[1].CompactionID)
	require.Equal(t, "manual", reports[0].Trigger)
	require.Equal(t, "manual", reports[1].Trigger)
}

type compactionFailureClient struct {
	*recorder
	failed bool
}

func (c *compactionFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if notification.Meta[wire.CompactionKey] != nil && !c.failed {
		c.failed = true

		return errors.New("compaction delivery unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestCompactionSendFailureKeepsRuntime(t *testing.T) {
	t.Parallel()
	a := NewAgent()
	rec := &compactionFailureClient{recorder: newRecorder()}
	a.attach(rec, nil)
	s := a.newSession(sessionStart{cwd: t.TempDir()})
	s.nativeID = "root"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt := &binding{cancel: cancel}
	s.runtime = rt
	for _, id := range []string{"failed", "next"} {
		s.handleEvent(ctx, rt, opencode.Event{ID: id, Type: "session.compacted", Properties: json.RawMessage(`{"sessionID":"root"}`)})
	}
	require.NoError(t, ctx.Err())
	require.True(t, rec.failed)
	require.Len(t, compactionReports(t, rec.snapshot()), 1)
}
