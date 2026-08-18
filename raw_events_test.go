package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const rawBoundaryField = "rawBoundaryData"

type failThenRecordRawClient struct {
	*recordingAgentClient
	failures int
}

func (c *failThenRecordRawClient) NotifyExtension(ctx context.Context, method string, params any) error {
	if c.failures > 0 {
		c.failures--

		return errors.New("client notify failed")
	}

	return c.recordingAgentClient.NotifyExtension(ctx, method, params)
}

// rawEventNotifications returns the event payloads (envelope maps) emitted for
// the given ACP session id, in order.
func rawEventNotifications(conn *recordingAgentClient, sessionID acp.SessionId) []map[string]any {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	out := make([]map[string]any, 0, len(conn.extensions))
	for _, ext := range conn.extensions {
		if ext.method != RawEventMethod {
			continue
		}
		payload, ok := ext.params.(map[string]any)
		if !ok || payload[jsonFieldSessionID] != sessionID {
			continue
		}
		out = append(out, payload)
	}

	return out
}

func rawEventSession(t *testing.T, id acp.SessionId, conn agentClient) *session {
	t.Helper()
	agent := NewAgent()
	agent.setAgentClient(conn)
	client := newFakeOpenCodeClient()
	sess := newSession(agent, id, "/tmp/project", nil, testNativeSession(string(id)), client, sessionMeta{}, idmapRecord{
		SessionID:       string(id),
		NativeSessionID: string(id),
		Format:          SessionStoreFormat,
	})
	sess.rawMessages = rawMessageConfig{enabled: true}

	return sess
}

func oversizeRawEvent() opencode.Event {
	big := strings.Repeat("x", rawEventMaxBytes+1024)

	return opencode.Event{Type: "message.updated", Raw: json.RawMessage(`{"blob":"` + big + `"}`)}
}

func normalRawEvent(marker string) opencode.Event {
	return opencode.Event{Type: "message.updated", Raw: json.RawMessage(`{"tag":"` + marker + `"}`)}
}

// The six functions below implement the raw-event spec from
// persona-raw-events.md (§Uniform test spec).

// Case 1 — oversized event emits exactly one fixed marker notification with the
// envelope intact.
func TestRawEventOversizeEmitsFixedMarker(t *testing.T) {
	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	ctx := withTurnRoute(context.Background(), "turn-raw")
	if err := sess.emitUpdate(ctx, acp.UpdateAgentMessageText("late")); err != nil {
		t.Fatalf("emit update: %v", err)
	}
	conn.mu.Lock()
	if !reflect.DeepEqual(conn.updates[0].Meta, routeCarrier("turn-raw")) {
		t.Fatalf("session/update route envelope = %#v", conn.updates[0].Meta)
	}
	conn.mu.Unlock()
	if err := sess.emitRawOpenCodeEvent(ctx, oversizeRawEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	events := rawEventNotifications(conn, "session-1")
	if len(events) != 1 {
		t.Fatalf("emitted %d notifications, want 1", len(events))
	}
	payload := events[0]
	if !reflect.DeepEqual(payload["_meta"], routeCarrier("turn-raw")) {
		t.Fatalf("route envelope = %#v", payload["_meta"])
	}
	if payload[jsonFieldSequence] != int64(1) || payload[jsonFieldSource] != rawEventSource {
		t.Fatalf("envelope not intact: %#v", payload)
	}
	marker, ok := payload[jsonFieldEvent].(map[string]any)
	if !ok {
		t.Fatalf("event is not a marker map: %#v", payload[jsonFieldEvent])
	}
	if marker[rawMarkerTruncated] != true || marker[rawMarkerReason] != rawReasonOversize {
		t.Fatalf("marker = %#v", marker)
	}
	if marker[rawMarkerMaxBytes] != rawEventMaxBytes {
		t.Fatalf("maxBytes = %#v, want %d", marker[rawMarkerMaxBytes], rawEventMaxBytes)
	}
	size, ok := marker[rawMarkerSizeBytes].(int)
	if !ok || size <= rawEventMaxBytes {
		t.Fatalf("sizeBytes = %#v, want int > %d", marker[rawMarkerSizeBytes], rawEventMaxBytes)
	}
}

// Case 2 — a mix of normal and oversized events yields a contiguous per-session
// sequence 1..N with no gaps.
func TestRawEventSequenceIsContiguous(t *testing.T) {
	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	const total = 5
	for i := 0; i < total; i++ {
		event := normalRawEvent("n")
		if i == 2 {
			event = oversizeRawEvent()
		}
		if err := sess.emitRawOpenCodeEvent(context.Background(), event); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	events := rawEventNotifications(conn, "session-1")
	if len(events) != total {
		t.Fatalf("emitted %d notifications, want %d", len(events), total)
	}
	for i, payload := range events {
		if payload[jsonFieldSequence] != int64(i+1) {
			t.Fatalf("sequence[%d] = %#v, want %d", i, payload[jsonFieldSequence], i+1)
		}
	}
}

// Nil payloads (missing, JSON null, or undecodable) are skipped WITHOUT
// consuming a sequence number: `"event": null` is never emitted and the
// sequence stays contiguous over the notifications that are actually emitted.
func TestRawEventNilPayloadSkippedWithoutSequence(t *testing.T) {
	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	events := []opencode.Event{
		normalRawEvent("first"),
		{Type: "message.updated"},
		{Type: "message.updated", Raw: json.RawMessage(`null`)},
		{Type: "message.updated", Raw: json.RawMessage(`{bad`)},
		normalRawEvent("second"),
	}
	for i, event := range events {
		if err := sess.emitRawOpenCodeEvent(context.Background(), event); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	emitted := rawEventNotifications(conn, "session-1")
	if len(emitted) != 2 {
		t.Fatalf("emitted %d notifications, want 2", len(emitted))
	}
	for i, payload := range emitted {
		if payload[jsonFieldSequence] != int64(i+1) {
			t.Fatalf("sequence[%d] = %#v, want %d", i, payload[jsonFieldSequence], i+1)
		}
		if payload[jsonFieldEvent] == nil {
			t.Fatalf("event[%d] payload is null: %#v", i, payload)
		}
	}
}

// Case 3 — two concurrent sessions each keep an independent sequence that starts
// at 1 and is contiguous.
func TestRawEventCrossSessionSequenceIsolation(t *testing.T) {
	conn := newRecordingAgentClient()
	sessA := rawEventSession(t, "session-a", conn)
	sessB := rawEventSession(t, "session-b", conn)
	for i := 0; i < 3; i++ {
		if err := sessA.emitRawOpenCodeEvent(context.Background(), normalRawEvent("a")); err != nil {
			t.Fatalf("emit a %d: %v", i, err)
		}
		if err := sessB.emitRawOpenCodeEvent(context.Background(), normalRawEvent("b")); err != nil {
			t.Fatalf("emit b %d: %v", i, err)
		}
	}
	for _, id := range []acp.SessionId{"session-a", "session-b"} {
		events := rawEventNotifications(conn, id)
		if len(events) != 3 {
			t.Fatalf("session %s emitted %d, want 3", id, len(events))
		}
		for i, payload := range events {
			if payload[jsonFieldSequence] != int64(i+1) {
				t.Fatalf("session %s sequence[%d] = %#v, want %d", id, i, payload[jsonFieldSequence], i+1)
			}
		}
	}
}

// Case 4 — every emitted event field is valid JSON, including an oversized event
// and one that fails to marshal (unserializable marker, no sizeBytes).
func TestRawEventValidJSONInvariant(t *testing.T) {
	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	if err := sess.emitRawOpenCodeEvent(context.Background(), normalRawEvent("ok")); err != nil {
		t.Fatalf("emit normal: %v", err)
	}
	if err := sess.emitRawOpenCodeEvent(context.Background(), oversizeRawEvent()); err != nil {
		t.Fatalf("emit oversize: %v", err)
	}
	for _, payload := range rawEventNotifications(conn, "session-1") {
		encoded, err := json.Marshal(payload[jsonFieldEvent])
		if err != nil || !json.Valid(encoded) {
			t.Fatalf("event field is not valid JSON: %#v err=%v", payload[jsonFieldEvent], err)
		}
	}

	marked, err := capRawEventPayload(map[string]any{
		jsonFieldSessionID: "session-1",
		jsonFieldSequence:  int64(9),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"bad": make(chan int)},
	})
	if err != nil {
		t.Fatalf("cap unserializable payload: %v", err)
	}
	marker, ok := marked[jsonFieldEvent].(map[string]any)
	if !ok || marker[rawMarkerReason] != rawReasonUnserializable {
		t.Fatalf("unserializable marker = %#v", marked[jsonFieldEvent])
	}
	if _, hasSize := marker[rawMarkerSizeBytes]; hasSize {
		t.Fatalf("unserializable marker unexpectedly carries sizeBytes: %#v", marker)
	}
	if encoded, err := json.Marshal(marker); err != nil || !json.Valid(encoded) {
		t.Fatalf("unserializable marker is not valid JSON: %#v err=%v", marker, err)
	}
}

func TestRawEventFinalPayloadBoundaryIncludesRouteMeta(t *testing.T) {
	payload := map[string]any{
		jsonFieldSessionID: "session-1",
		jsonFieldSequence:  int64(1),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{rawBoundaryField: ""},
		"_meta":            routeCarrier(strings.Repeat("n", routeTurnNonceMaxBytes)),
	}
	empty, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal empty payload: %v", err)
	}
	padding := rawEventMaxBytes - len(empty)
	if padding <= 0 {
		t.Fatalf("empty routed payload is %d bytes", len(empty))
	}
	payload[jsonFieldEvent] = map[string]any{rawBoundaryField: strings.Repeat("x", padding)}

	capped, err := capRawEventPayload(payload)
	if err != nil {
		t.Fatalf("cap boundary payload: %v", err)
	}
	encoded, err := json.Marshal(capped)
	if err != nil {
		t.Fatalf("marshal boundary payload: %v", err)
	}
	if len(encoded) != rawEventMaxBytes {
		t.Fatalf("boundary payload = %d bytes, want %d", len(encoded), rawEventMaxBytes)
	}
	if event, ok := capped[jsonFieldEvent].(map[string]any); !ok || event[rawMarkerTruncated] == true {
		t.Fatalf("exact-boundary event was replaced: %#v", capped[jsonFieldEvent])
	}

	payload[jsonFieldEvent] = map[string]any{rawBoundaryField: strings.Repeat("x", padding+1)}
	capped, err = capRawEventPayload(payload)
	if err != nil {
		t.Fatalf("cap over-boundary payload: %v", err)
	}
	encoded, err = json.Marshal(capped)
	if err != nil {
		t.Fatalf("marshal capped payload: %v", err)
	}
	if len(encoded) > rawEventMaxBytes {
		t.Fatalf("capped payload = %d bytes, exceeds %d", len(encoded), rawEventMaxBytes)
	}
	marker, ok := capped[jsonFieldEvent].(map[string]any)
	if !ok {
		t.Fatalf("over-boundary event is not a marker: %#v", capped[jsonFieldEvent])
	}
	if marker[rawMarkerReason] != rawReasonOversize || marker[rawMarkerSizeBytes] != rawEventMaxBytes+1 {
		t.Fatalf("over-boundary marker = %#v", marker)
	}
}

func TestRawEventFinalPayloadRejectsUnboundedInternalRoute(t *testing.T) {
	hugeNonce := strings.Repeat("n", rawEventMaxBytes)
	payload := map[string]any{
		jsonFieldSessionID: "session-1",
		jsonFieldSequence:  int64(1),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"type": "event"},
		"_meta":            routeCarrier(hugeNonce),
	}

	if _, err := capRawEventPayload(payload); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded route error = %v", err)
	}

	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	if err := sess.emitRawOpenCodeEvent(withTurnRoute(context.Background(), hugeNonce), normalRawEvent("event")); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("emit with unbounded internal route error = %v", err)
	}
	if sess.rawSeq != 0 || len(rawEventNotifications(conn, "session-1")) != 0 {
		t.Fatalf("failed cap changed sequence or emitted: sequence=%d events=%#v", sess.rawSeq, rawEventNotifications(conn, "session-1"))
	}

	payload["_meta"] = map[string]any{"bad": make(chan int)}
	if _, err := capRawEventPayload(payload); err == nil || !strings.Contains(err.Error(), "marshal capped") {
		t.Fatalf("unserializable final payload error = %v", err)
	}
}

// Case 5 — a NotifyExtension failure is recorded internally and does NOT abort
// the prompt turn.
func TestRawEventEmitFailureDoesNotFailTurn(t *testing.T) {
	conn := newRecordingAgentClient()
	failing := &failThenRecordRawClient{recordingAgentClient: conn, failures: 1}
	sess := rawEventSession(t, "session-1", failing)
	sess.routeNativeEvent(context.Background(), normalRawEvent("boom"))
	if sess.rawSeq != 0 {
		t.Fatalf("failed delivery consumed sequence %d", sess.rawSeq)
	}
	if err := sess.emitRawOpenCodeEvent(context.Background(), normalRawEvent("ok")); err != nil {
		t.Fatalf("successful emit after failure: %v", err)
	}
	events := rawEventNotifications(conn, "session-1")
	if len(events) != 1 || events[0][jsonFieldSequence] != int64(1) {
		t.Fatalf("successful events after failure = %#v", events)
	}
}

// Case 6 — with raw events default-off, no notifications are emitted regardless
// of native event volume.
func TestRawEventDefaultOffEmitsNothing(t *testing.T) {
	conn := newRecordingAgentClient()
	sess := rawEventSession(t, "session-1", conn)
	sess.rawMessages = rawMessageConfig{}
	for i := 0; i < 4; i++ {
		if err := sess.emitRawOpenCodeEvent(context.Background(), oversizeRawEvent()); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if events := rawEventNotifications(conn, "session-1"); len(events) != 0 {
		t.Fatalf("default-off emitted %d notifications, want 0", len(events))
	}
}

// TestSessionPromptBackpressureLimitString pins the exact -32600 backpressure
// payload for the session_prompt turn limit.
func TestSessionPromptBackpressureLimitString(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	agent := NewAgent()
	session := testSession(agent, client)

	release, err := session.acquireTurn(ctx)
	if err != nil {
		t.Fatalf("first acquireTurn: %v", err)
	}
	defer release()

	_, err = session.acquireTurn(ctx)
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("backpressure error = %v, want RequestError", err)
	}
	if reqErr.Code != -32600 {
		t.Fatalf("backpressure code = %d, want -32600", reqErr.Code)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("backpressure data = %#v, want map", reqErr.Data)
	}
	if data[jsonFieldError] != errValueBackpressure {
		t.Fatalf("backpressure error tag = %#v, want %q", data[jsonFieldError], errValueBackpressure)
	}
	if data[jsonFieldLimit] != limitSessionPrompt {
		t.Fatalf("backpressure limit = %#v, want %q", data[jsonFieldLimit], limitSessionPrompt)
	}
}
