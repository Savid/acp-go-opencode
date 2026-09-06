package opencodeacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// jsonTestPath spells an absolute test path as a JSON string value, so a raw
// JSON fixture carries the same absolute path the surrounding Go code passes.
func jsonTestPath(segments ...string) string {
	encoded, err := json.Marshal(absTestPath(segments...))
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

func syncTestEvent(aggregate string, sequence int64, kind string, extra map[string]json.RawMessage) opencode.SyncEvent {
	data := map[string]json.RawMessage{
		"sessionID": json.RawMessage(`"` + aggregate + `"`),
		"info":      json.RawMessage(`{"id":"` + aggregate + `","directory":` + jsonTestPath("source") + `}`),
	}
	for key, value := range extra {
		data[key] = value
	}

	return opencode.SyncEvent{ID: aggregate + "-evt", AggregateID: aggregate, Sequence: sequence, Type: kind, Data: data}
}

func TestAllowlistedSyncEventsRejectsCrossAggregateAndUnknownSchema(t *testing.T) {
	allow := map[string]stateSnapshotNode{"a": {NativeSessionID: "a", SourceCwd: absTestPath("source")}}
	history := []opencode.SyncEvent{
		syncTestEvent("a", 0, "session.created.1", nil),
		syncTestEvent("other", 0, "session.created.1", nil),
	}
	events, cursors, err := allowlistedSyncEvents(history, allow)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Len(t, events["a"], 1)
	require.EqualValues(t, 0, cursors["a"])

	bad := syncTestEvent("a", 0, "session.created.1", map[string]json.RawMessage{"credential": json.RawMessage(`"secret"`)})
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{bad}, allow)
	require.ErrorContains(t, err, "unsupported field")
	bad.Type = "future.2"
	delete(bad.Data, "credential")
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{bad}, allow)
	require.ErrorContains(t, err, "unsupported sync event type")
}

func TestSyncSnapshotHardRejectsForeignAndIncompleteFormats(t *testing.T) {
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(context.Background(), SessionKey{SessionID: "s"}, []SessionStoreEntry{
		json.RawMessage(`{"format":"unknown-format"}`),
	}))
	idmap, hydrated, found, err := hydrateStateFromStore(context.Background(), store, "s")
	require.Empty(t, idmap)
	require.Empty(t, hydrated)
	require.False(t, found)
	require.ErrorContains(t, err, "missing required field")

	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))
	delete(snapshot.Events, "native")
	require.ErrorContains(t, validateSyncSnapshot("s", snapshot), "incomplete")
}

func TestHydrateStateSnapshotRejectsAmbiguousJSONAtEveryTypedDepth(t *testing.T) {
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	snapshot.Session.Model = stateSnapshotModel{ProviderID: "openai", ModelID: "gpt-test", Agent: "build"}
	snapshot.Session.Env = map[string]string{"SERVICE_TOKEN": "credential"}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	valid := string(encoded)
	tests := map[string]string{
		"unknown top-level field": strings.Replace(valid, `"format":`, `"future":true,"format":`, 1),
		"unknown session field":   strings.Replace(valid, `"sessionId":`, `"future":true,"sessionId":`, 1),
		"unknown model field":     strings.Replace(valid, `"providerID":`, `"future":true,"providerID":`, 1),
		"unknown graph field":     strings.Replace(valid, `"sourceCwd":`, `"future":true,"sourceCwd":`, 1),
		"unknown event field":     strings.Replace(valid, `"aggregate_id":`, `"future":true,"aggregate_id":`, 1),
		"case-folded field":       strings.Replace(valid, `"format":`, `"Format":`, 1),
		"duplicate top-level":     strings.Replace(valid, `"format":`, `"format":"shadow","format":`, 1),
		"duplicate session":       strings.Replace(valid, `"sessionId":`, `"sessionId":"shadow","sessionId":`, 1),
		"duplicate model":         strings.Replace(valid, `"providerID":`, `"providerID":"shadow","providerID":`, 1),
		"duplicate graph":         strings.Replace(valid, `"sourceCwd":`, `"sourceCwd":"/shadow","sourceCwd":`, 1),
		"duplicate event":         strings.Replace(valid, `"aggregate_id":`, `"aggregate_id":"shadow","aggregate_id":`, 1),
		"duplicate event data":    strings.Replace(valid, `"sessionID":`, `"sessionID":"shadow","sessionID":`, 1),
		"duplicate nested data": strings.Replace(valid,
			`"directory":`+jsonTestPath("source"),
			`"directory":"/shadow","directory":`+jsonTestPath("source"), 1),
		"trailing input": valid + ` {}`,
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewInMemorySessionStore()
			require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
				Key:     SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath},
				Entries: []SessionStoreEntry{json.RawMessage(raw)},
			}}))

			_, _, found, err := hydrateStateFromStore(t.Context(), store, "session")
			require.Error(t, err)
			require.False(t, found)
		})
	}
}

func TestHydrateStateSnapshotRequiresEveryNonOmittedMemberBeforeNativeLaunch(t *testing.T) {
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	snapshot.Session.Model = stateSnapshotModel{ProviderID: "openai", ModelID: "gpt-test", Agent: "build"}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	var original map[string]any
	require.NoError(t, json.Unmarshal(encoded, &original))

	requiredPaths := [][]any{
		{"format"}, {"adapterVersion"}, {"nativeVersion"}, {"eventSchemaVersion"},
		{"capturedAtUnixMilli"}, {"restoreGeneration"}, {"session"}, {"graph"}, {"events"},
		{"session", "sessionId"}, {"session", "nativeSessionId"}, {"session", "cwd"},
		{"session", "title"}, {"session", "model"}, {"session", "env"}, {"session", "extraPathDirs"},
		{"graph", 0, "sessionId"}, {"graph", 0, "nativeSessionId"},
		{"graph", 0, "sourceCwd"}, {"graph", 0, "permission"},
		{"events", "native", 0, "id"}, {"events", "native", 0, "aggregate_id"},
		{"events", "native", 0, "seq"}, {"events", "native", 0, "type"},
		{"events", "native", 0, "data"},
	}

	for _, path := range requiredPaths {
		name := fmt.Sprint(path)
		t.Run(name, func(t *testing.T) {
			candidate := cloneJSONMap(t, original)
			deleteJSONPath(t, candidate, path)
			raw, marshalErr := json.Marshal(candidate)
			require.NoError(t, marshalErr)

			store := NewInMemorySessionStore()
			require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
				Key: SessionKey{SessionID: "session"}, Entries: []SessionStoreEntry{raw},
			}}))
			client := newFakeOpenCodeClient(t)
			client.getSession = testNativeSession("native")
			agent := NewAgent(WithSessionStore(store))
			agent.runtime = client

			_, resumeErr := agent.ResumeSession(t.Context(), ResumeSessionRequest("session", absTestPath("target")))
			require.Error(t, resumeErr)
			require.Empty(t, client.scopes(), "invalid snapshot reached native launch")
		})
	}

	optional := cloneJSONMap(t, original)
	deleteJSONPath(t, optional, []any{"session", "parentSessionId"})
	deleteJSONPath(t, optional, []any{"session", "nativeParentSessionId"})
	deleteJSONPath(t, optional, []any{"session", "model", "providerID"})
	deleteJSONPath(t, optional, []any{"session", "model", "modelID"})
	deleteJSONPath(t, optional, []any{"session", "model", "agent"})
	deleteJSONPath(t, optional, []any{"graph", 0, "parentSessionId"})
	deleteJSONPath(t, optional, []any{"graph", 0, "nativeParentId"})
	optionalRaw, err := json.Marshal(optional)
	require.NoError(t, err)
	require.NoError(t, decodeSnapshotOnly(optionalRaw))
}

func TestSyncEventSchemasRequireMandatoryKeysAndTypes(t *testing.T) {
	node := stateSnapshotNode{NativeSessionID: "native"}
	valid := map[string]opencode.SyncEvent{
		"session.created.1": syncSchemaTestEvent("session.created.1", map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`), syncFieldInfo: json.RawMessage(`{"id":"native"}`),
		}),
		"session.updated.1": syncSchemaTestEvent("session.updated.1", map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`), syncFieldInfo: json.RawMessage(`{"id":"native"}`),
		}),
		"message.updated.1": syncSchemaTestEvent("message.updated.1", map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`), syncFieldInfo: json.RawMessage(`{"id":"message"}`),
		}),
		"message.part.updated.1": syncSchemaTestEvent("message.part.updated.1", map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`), syncFieldPart: json.RawMessage(`{"id":"part"}`),
			jsonFieldTime: json.RawMessage(`123.5`),
		}),
	}

	for eventType, event := range valid {
		t.Run(eventType+" valid", func(t *testing.T) {
			require.NoError(t, validateSyncEvent(event, node))
		})
		for _, field := range syncEventDataSchemas[eventType].required {
			t.Run(eventType+" missing "+field, func(t *testing.T) {
				candidate := cloneSyncEvent(event)
				delete(candidate.Data, field)
				require.ErrorContains(t, validateSyncEvent(candidate, node), "missing required field")
			})
		}
	}

	wrongTypes := map[string]struct {
		eventType string
		field     string
		value     json.RawMessage
	}{
		"session id number":  {"session.created.1", syncFieldSessionID, json.RawMessage(`1`)},
		"session info array": {"session.updated.1", syncFieldInfo, json.RawMessage(`[]`)},
		"message info null":  {"message.updated.1", syncFieldInfo, json.RawMessage(`null`)},
		"part array":         {"message.part.updated.1", syncFieldPart, json.RawMessage(`[]`)},
		"time string":        {"message.part.updated.1", jsonFieldTime, json.RawMessage(`"now"`)},
		"time null":          {"message.part.updated.1", jsonFieldTime, json.RawMessage(`null`)},
	}
	for name, test := range wrongTypes {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSyncEvent(valid[test.eventType])
			candidate.Data[test.field] = test.value
			require.Error(t, validateSyncEvent(candidate, node))
		})
	}
}

func syncSchemaTestEvent(eventType string, data map[string]json.RawMessage) opencode.SyncEvent {
	return opencode.SyncEvent{ID: "event", AggregateID: "native", Type: eventType, Data: data}
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	var cloned map[string]any
	require.NoError(t, json.Unmarshal(raw, &cloned))

	return cloned
}

func deleteJSONPath(t *testing.T, root map[string]any, path []any) {
	t.Helper()
	var current any = root
	for _, component := range path[:len(path)-1] {
		switch typed := component.(type) {
		case string:
			object, ok := current.(map[string]any)
			require.True(t, ok)
			current, ok = object[typed]
			require.True(t, ok)
		case int:
			array, ok := current.([]any)
			require.True(t, ok)
			require.Less(t, typed, len(array))
			current = array[typed]
		}
	}

	object, ok := current.(map[string]any)
	require.True(t, ok)
	name, ok := path[len(path)-1].(string)
	require.True(t, ok)
	delete(object, name)
}

func decodeSnapshotOnly(raw []byte) error {
	_, err := decodeStateSnapshot(raw)

	return err
}

func TestRestoreRebasesAndVerifiesExactEventSet(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.getSession = testNativeSession("native")
	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))
	native, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.NoError(t, err)
	require.Equal(t, "native", native.ID)
	require.Len(t, client.syncEvents, 1)
	var info map[string]any
	require.NoError(t, json.Unmarshal(client.syncEvents[0].Data["info"], &info))
	require.Equal(t, absTestPath("target"), info["directory"])
	_, err = os.Stat(filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName))
	require.NoError(t, err)

	// A complete retry is idempotent and verifies rather than duplicating.
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 1)

	// Content this bundle did not write is refused: the destination and the
	// stored generation are the same length and disagree, so neither is the
	// other's prefix.
	client.syncEvents[0].Data["info"] = json.RawMessage(`{"id":"foreign","directory":` + jsonTestPath("target") + `}`)
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "diverge from the stored generation")
}

// TestRestoreAcceptsNativeEventsAppendedAfterTheCapture proves a stored
// generation stays restorable once the harness has written to the aggregate
// again. OpenCode settles a session title and touches timestamps after a turn
// ends, so a destination that is ahead of the capture is the ordinary case, not
// a conflict — the stored generation only has to be present as the ordered
// prefix.
func TestRestoreAcceptsNativeEventsAppendedAfterTheCapture(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.getSession = testNativeSession("native")
	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))

	_, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 1)

	// The harness appends one more event of its own after the capture.
	later := cloneSyncEvent(client.syncEvents[0])
	later.ID = "evt-later"
	later.Sequence = 1
	later.Type = syncTypeSessionUpdated
	client.syncEvents = append(client.syncEvents, later)

	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 2, "a later native event is neither replayed over nor removed")
}

func TestRestoreComparesExistingNativeCarrierThroughPortableProjection(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.getSession = testNativeSession("native")
	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))

	nativeEvent := cloneSyncEvent(snapshot.Events["native"][0])
	nativeEvent.Data[syncFieldInfo] = json.RawMessage(`{
		"id":"native",
		"directory":` + jsonTestPath("source") + `,
		"metadata":{
			"native":{"kept":true},
			"acp-go-opencode":{"ref":"stale-reference"}
		}
	}`)
	portable, err := portableSyncEvents([]opencode.SyncEvent{nativeEvent})
	require.NoError(t, err)
	snapshot.Events["native"] = portable
	require.NoError(t, recordSnapshotOwnership(client, snapshot))
	client.syncEvents = []opencode.SyncEvent{nativeEvent}

	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 1, "matching native history must be verified, not replayed")
	require.Contains(t, string(client.syncEvents[0].Data[syncFieldInfo]), "stale-reference",
		"portable comparison must not mutate the live native event")
}

func TestRestoreRejectsMalformedCarrierInExistingAndVerifiedNativeHistory(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		client := newFakeOpenCodeClient(t)
		snapshot := validSyncSnapshot("s", "native", absTestPath("source"))
		require.NoError(t, recordSnapshotOwnership(client, snapshot))
		malformed := cloneSyncEvent(snapshot.Events["native"][0])
		malformed.Data[syncFieldInfo] = json.RawMessage(`{"metadata":"not-an-object"}`)
		client.syncEvents = []opencode.SyncEvent{malformed}

		_, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
		require.ErrorContains(t, err, "sanitize sync event")
	})

	t.Run("verified after replay", func(t *testing.T) {
		client := newFakeOpenCodeClient(t)
		snapshot := validSyncSnapshot("s", "native", absTestPath("source"))
		historyCalls := 0
		client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
			historyCalls++
			if historyCalls == 1 {
				return nil, nil
			}

			malformed := cloneSyncEvent(snapshot.Events["native"][0])
			malformed.Data[syncFieldInfo] = json.RawMessage(`{"metadata":"not-an-object"}`)

			return []opencode.SyncEvent{malformed}, nil
		}

		_, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
		require.ErrorContains(t, err, "sanitize sync event")
	})
}

func TestRestoreRejectsExistingAggregateWithoutDurableOwner(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.syncEvents = []opencode.SyncEvent{syncTestEvent("native", 0, "session.created.1", nil)}
	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))

	_, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("source"))
	require.ErrorContains(t, err, "no durable restore owner")
}

func TestRestoreRejectsPathOutsideCapturedCWD(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	snapshot := validSyncSnapshot("s", "native", absTestPath("source"))
	snapshot.Events["native"][0].Data["info"] = json.RawMessage(`{"id":"native","directory":` + jsonTestPath("other") + `}`)

	_, err := restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "escapes source cwd")
	require.Empty(t, client.syncEvents)
}

func TestBundleCredentialScan(t *testing.T) {
	require.NoError(t, scanSyncBundle([]byte(`{"text":"ordinary"}`), nil))
	require.ErrorContains(t, scanSyncBundle([]byte(`{"authorization":"Bearer secret"}`), nil), "forbidden")
	require.ErrorContains(t, scanSyncBundle([]byte(`{"text":"Bearer exact"}`), []string{"Bearer exact"}), "MCP credential")
}

func TestSnapshotCredentialScanAllowsOnlyTheDurableCarrier(t *testing.T) {
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	snapshot.Session.Env = map[string]string{"SERVICE_API_TOKEN": "bearer-secret", "EMPTY": ""}
	require.NoError(t, scanStateSnapshot(snapshot, []string{"bearer-secret"}))

	snapshot.Events["native"][0].Data[syncFieldInfo] = json.RawMessage(`{"id":"native","leak":"bearer-secret"}`)
	require.ErrorContains(t, scanStateSnapshot(snapshot, []string{"bearer-secret"}), "MCP credential")
}

func TestReadSyncGenerationRemovesOnlyNativeSessionCarrierReference(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient(t)
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	client.syncEvents[0].Data[syncFieldInfo] = json.RawMessage(`{
		"id":"native-1",
		"directory":` + jsonTestPath("source") + `,
		"metadata":{
			"native":{"kept":true},
			"acp-go-opencode":{"ref":"opaque-operation-reference"}
		}
	}`)

	events, err := current.readSyncGeneration(
		context.Background(),
		map[string]stateSnapshotNode{"native-1": {
			SessionID: string(current.id), NativeSessionID: "native-1", SourceCwd: absTestPath("source"),
		}},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, events["native-1"], 1)
	require.NotContains(t, string(events["native-1"][0].Data[syncFieldInfo]), "opaque-operation-reference")
	require.NotContains(t, string(events["native-1"][0].Data[syncFieldInfo]), "acp-go-opencode")
	require.Contains(t, string(events["native-1"][0].Data[syncFieldInfo]), `"native":{"kept":true}`)

	bundle, err := json.Marshal(events)
	require.NoError(t, err)
	require.NoError(t, scanSyncBundle(bundle, current.secretNeedles))
}

func validSyncSnapshot(sessionID, nativeID, cwd string) stateSnapshot {
	event := syncTestEvent(nativeID, 0, "session.created.1", nil)

	return stateSnapshot{
		Format: SessionStoreFormat, AdapterVersion: "test", NativeVersion: minNativeVersion,
		EventSchemaVersion: syncEventSchemaVersion, RestoreGeneration: "generation",
		Session: stateSnapshotSession{
			SessionID: sessionID, NativeSessionID: nativeID, Cwd: cwd,
			Env: map[string]string{}, ExtraPathDirs: []string{},
		},
		Graph:  []stateSnapshotNode{{SessionID: sessionID, NativeSessionID: nativeID, SourceCwd: cwd, Permission: "ask"}},
		Events: map[string][]opencode.SyncEvent{nativeID: {event}},
	}
}
func TestSyncSnapshotValidationEveryFailureShape(t *testing.T) {
	base := validSyncSnapshot("session", "native", absTestPath("source"))
	require.NoError(t, validateSyncSnapshot("session", base))

	tests := map[string]func(*stateSnapshot){
		"format":               func(value *stateSnapshot) { value.Format = "old" },
		"event schema":         func(value *stateSnapshot) { value.EventSchemaVersion = "old" },
		"session identity":     func(value *stateSnapshot) { value.Session.SessionID = "other" },
		"native identity":      func(value *stateSnapshot) { value.Session.NativeSessionID = "" },
		"generation":           func(value *stateSnapshot) { value.RestoreGeneration = "" },
		"missing environment":  func(value *stateSnapshot) { value.Session.Env = nil },
		"invalid environment":  func(value *stateSnapshot) { value.Session.Env = map[string]string{"PATH": "/unsafe"} },
		"missing path carrier": func(value *stateSnapshot) { value.Session.ExtraPathDirs = nil },
		"invalid path carrier": func(value *stateSnapshot) { value.Session.ExtraPathDirs = []string{"relative"} },
		"invalid node":         func(value *stateSnapshot) { value.Graph[0].SourceCwd = "" },
		"duplicate aggregate":  func(value *stateSnapshot) { value.Graph = append(value.Graph, value.Graph[0]) },
		"selected absent": func(value *stateSnapshot) {
			value.Graph[0].NativeSessionID = "other"
			value.Events = map[string][]opencode.SyncEvent{"other": value.Events["native"]}
		},
		"unknown event aggregate": func(value *stateSnapshot) { value.Events["other"] = value.Events["native"] },
		"empty event aggregate":   func(value *stateSnapshot) { value.Events["native"] = nil },
		"invalid event": func(value *stateSnapshot) {
			value.Events["native"][0].Type = "unknown"
		},
		"incomplete graph": func(value *stateSnapshot) { value.Events = map[string][]opencode.SyncEvent{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := cloneStateSnapshot(t, base)
			mutate(&value)
			require.Error(t, validateSyncSnapshot("session", value))
		})
	}
}

// TestSyncSnapshotRejectsNonCanonicalGraphAndEventOrder pins the shape of a
// stored bundle's graph. A bundle carries exactly one node — its own session —
// because an OpenCode fork owns an independent aggregate and no session's
// restore needs another session's events. The node's fork lineage is metadata:
// it may name a parent the bundle does not contain, and it may never disagree
// with the carrier it belongs to.
func TestSyncSnapshotRejectsNonCanonicalGraphAndEventOrder(t *testing.T) {
	base := validSyncSnapshot("session", "native", absTestPath("source"))
	require.NoError(t, validateSyncSnapshot("session", base))

	// A fork's own bundle names the lineage it branched from and restores from
	// its own aggregate alone, whether or not the parent is still loaded.
	fork := validSyncSnapshot("fork", "fork-native", absTestPath("source"))
	fork.Session.ParentSessionID, fork.Session.NativeParentSessionID = "session", "native"
	fork.Graph[0].ParentSessionID, fork.Graph[0].NativeParentID = "session", "native"
	require.NoError(t, validateSyncSnapshot("fork", fork))

	tests := map[string]func(*stateSnapshot){
		"empty graph": func(value *stateSnapshot) {
			value.Graph = nil
		},
		"second node": func(value *stateSnapshot) {
			value.Graph = append(value.Graph, stateSnapshotNode{
				SessionID: "child", NativeSessionID: "child-native",
				ParentSessionID: "session", NativeParentID: "native",
				SourceCwd: absTestPath("source"), Permission: "ask",
			})
			value.Events["child-native"] = []opencode.SyncEvent{syncTestEvent("child-native", 0, "session.created.1", nil)}
		},
		"half parent identity": func(value *stateSnapshot) {
			value.Graph[0].ParentSessionID = "parent"
		},
		"selected logical mismatch": func(value *stateSnapshot) {
			value.Graph[0].SessionID = "other"
		},
		"selected parent mismatch": func(value *stateSnapshot) {
			value.Session.ParentSessionID = "parent"
			value.Session.NativeParentSessionID = "parent-native"
		},
		"selected native parent mismatch": func(value *stateSnapshot) {
			value.Graph[0].ParentSessionID, value.Graph[0].NativeParentID = "parent", "parent-native"
			value.Session.ParentSessionID = "parent"
		},
		"selected cwd mismatch": func(value *stateSnapshot) {
			value.Session.Cwd = absTestPath("other")
		},
		"event sequence gap": func(value *stateSnapshot) {
			value.Events["native"][0].Sequence = 1
		},
		"event sequence reordered": func(value *stateSnapshot) {
			one := cloneSyncEvent(value.Events["native"][0])
			one.ID = "event-one"
			one.Sequence = 1
			value.Events["native"] = []opencode.SyncEvent{one, value.Events["native"][0]}
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := cloneStateSnapshot(t, base)
			mutate(&value)
			require.Error(t, validateSyncSnapshot("session", value))
		})
	}
}

func TestSyncEventAllowlistAndIdentityBranches(t *testing.T) {
	node := stateSnapshotNode{SessionID: "session", NativeSessionID: "native", SourceCwd: absTestPath("source")}
	event := opencode.SyncEvent{
		ID: "event", AggregateID: "native", Sequence: 0, Type: "session.created.1",
		Data: map[string]json.RawMessage{"sessionID": json.RawMessage(`"native"`), "info": json.RawMessage(`{"id":"native"}`)},
	}
	require.NoError(t, validateSyncEvent(event, node))

	for name, mutate := range map[string]func(*opencode.SyncEvent){
		"empty id":        func(value *opencode.SyncEvent) { value.ID = "" },
		"wrong aggregate": func(value *opencode.SyncEvent) { value.AggregateID = "other" },
		"negative":        func(value *opencode.SyncEvent) { value.Sequence = -1 },
		"unknown type":    func(value *opencode.SyncEvent) { value.Type = "unknown" },
		"unknown field":   func(value *opencode.SyncEvent) { value.Data["secret"] = json.RawMessage(`true`) },
		"invalid session": func(value *opencode.SyncEvent) { value.Data["sessionID"] = json.RawMessage(`{`) },
		"wrong session":   func(value *opencode.SyncEvent) { value.Data["sessionID"] = json.RawMessage(`"other"`) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSyncEvent(event)
			mutate(&candidate)
			require.Error(t, validateSyncEvent(candidate, node))
		})
	}

	allow := map[string]stateSnapshotNode{"native": node}
	grouped, cursors, err := allowlistedSyncEvents([]opencode.SyncEvent{
		{ID: "ignored", AggregateID: "other"}, event,
	}, allow)
	require.NoError(t, err)
	require.Len(t, grouped["native"], 1)
	require.EqualValues(t, 0, cursors["native"])

	_, _, err = allowlistedSyncEvents(nil, allow)
	require.ErrorContains(t, err, "missing aggregate")
	noncontiguous := cloneSyncEvent(event)
	noncontiguous.Sequence = 1
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{noncontiguous}, allow)
	require.ErrorContains(t, err, "non-contiguous")
	invalid := cloneSyncEvent(event)
	invalid.Type = "unknown"
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{invalid}, allow)
	require.Error(t, err)

	sequenceOne := cloneSyncEvent(event)
	sequenceOne.ID = "event-one"
	sequenceOne.Sequence = 1
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{sequenceOne, event}, allow)
	require.NoError(t, err)
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{event, sequenceOne}, allow)
	require.NoError(t, err)
	equalSequence := cloneSyncEvent(event)
	equalSequence.ID = "event-other"
	_, _, err = allowlistedSyncEvents([]opencode.SyncEvent{event, equalSequence}, allow)
	require.ErrorContains(t, err, "non-contiguous")
}

func TestHydrateRebaseAndSyncComparisonBranches(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	_, _, ok, err := hydrateStateFromStore(ctx, store, "missing")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "bad"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "bad", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{json.RawMessage(`{`)},
	}}))
	badIDMap, badSnapshot, badFound, err := hydrateStateFromStore(ctx, store, "bad")
	require.Error(t, err)
	require.Empty(t, badIDMap)
	require.Empty(t, badSnapshot)
	require.False(t, badFound)

	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{encoded},
	}}))
	idmap, loaded, ok, err := hydrateStateFromStore(ctx, store, "session")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "native", idmap.NativeSessionID)
	require.Equal(t, snapshot.RestoreGeneration, loaded.RestoreGeneration)

	value := map[string]any{
		"directory": absTestPath("source"), "cwd": absTestPath("source", "sub"),
		"root": "relative", "other": absTestPath("source", "ignored"),
		"nested": []any{map[string]any{"path": absTestPath("source", "file")}, true},
	}
	rebased, err := rebasePathValues(value, "", absTestPath("source"), absTestPath("target"))
	require.NoError(t, err)
	result, ok := rebased.(map[string]any)
	require.True(t, ok)
	require.Equal(t, absTestPath("target"), result["directory"])
	require.Equal(t, absTestPath("target", "sub"), result["cwd"])
	require.Equal(t, "relative", result["root"])
	require.Equal(t, absTestPath("source", "ignored"), result["other"])
	_, err = rebasePathValues(absTestPath("outside"), "path", absTestPath("source"), absTestPath("target"))
	require.ErrorContains(t, err, "escapes source cwd")

	badEvent := cloneSyncEvent(snapshot.Events["native"][0])
	badEvent.Data["info"] = json.RawMessage(`{`)
	_, err = rebaseSyncEvents([]opencode.SyncEvent{badEvent}, absTestPath("source"), absTestPath("target"))
	require.Error(t, err)
	samePathEvent := cloneSyncEvent(snapshot.Events["native"][0])
	samePathEvent.Data["info"] = json.RawMessage(`{ "directory": ` + jsonTestPath("source") + `, "id": "native" }`)
	samePath, err := rebaseSyncEvents([]opencode.SyncEvent{samePathEvent},
		absTestPath("source")+string(filepath.Separator)+".", absTestPath("source"))
	require.NoError(t, err)
	require.Equal(t, samePathEvent.Data["info"], samePath[0].Data["info"], "same-path restore must preserve native JSON bytes")

	event := snapshot.Events["native"][0]
	// Agreement is symmetric: a destination behind the stored generation and one
	// ahead of it both describe the same history, and a differing event does not.
	require.True(t, syncEventsAgree(nil, []opencode.SyncEvent{event}))
	require.True(t, syncEventsAgree([]opencode.SyncEvent{event, event}, []opencode.SyncEvent{event}))
	require.True(t, syncEventsAgree([]opencode.SyncEvent{event}, []opencode.SyncEvent{event, event}))
	diverged := cloneSyncEvent(event)
	diverged.ID = "diverged"
	require.False(t, syncEventsAgree([]opencode.SyncEvent{diverged, event}, []opencode.SyncEvent{event}))
	require.False(t, syncEventsEqual(nil, []opencode.SyncEvent{event}))
	changed := cloneSyncEvent(event)
	changed.ID = "changed"
	require.False(t, syncEventsEqual([]opencode.SyncEvent{event}, []opencode.SyncEvent{changed}))
	sequenceOne := cloneSyncEvent(event)
	sequenceOne.Sequence = 1
	require.False(t, syncEventsEqual([]opencode.SyncEvent{sequenceOne, event}, []opencode.SyncEvent{event, sequenceOne}))
}

func TestSnapshotBlockSecretsAndGenerationBranches(t *testing.T) {
	agent := NewAgent(WithEnv(map[string]string{
		"API_TOKEN": "token", "PASSWORD": "password", "COOKIE": "cookie", "EMPTY_TOKEN": "", "NORMAL": "ignored",
	}))
	client := newFakeOpenCodeClient(t)
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current

	registry := testIncarnation(current).registry
	claimed, err := registry.claim(&pendingAction{id: "permission"})
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, metaPermissionKey, current.snapshotBlockedReason())
	_, held := registry.take("permission")
	require.True(t, held)
	current.activeMessageIDs["message"] = struct{}{}
	require.Equal(t, "generation", current.snapshotBlockedReason())
	delete(current.activeMessageIDs, "message")
	require.Empty(t, current.snapshotBlockedReason())

	current.secretNeedles = []string{"mcp-secret"}
	needles := agent.graphSecretNeedles([]*session{current})
	require.ElementsMatch(t, []string{"mcp-secret", "token", "password", "cookie"}, needles)

	oldRead := restoreRandRead
	restoreRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	t.Cleanup(func() { restoreRandRead = oldRead })
	_, err = newRestoreGeneration()
	require.ErrorContains(t, err, "entropy failed")
	restoreRandRead = oldRead
	generation, err := newRestoreGeneration()
	require.NoError(t, err)
	require.Len(t, generation, 32)
}

func TestSnapshotToStoreRemainingFailureStages(t *testing.T) {
	newSnapshotSession := func() (*session, *fakeOpenCodeClient) {
		agent := NewAgent()
		client := newFakeOpenCodeClient(t)
		current := testSession(t, agent, client)
		agent.sessions[current.id] = current

		return current, client
	}

	current, _ := newSnapshotSession()
	claimed, err := testIncarnation(current).registry.claim(&pendingAction{id: "permission"})
	require.NoError(t, err)
	require.True(t, claimed)
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "permission")

	current, client := newSnapshotSession()
	client.syncEvents[0].Type = "unknown"
	require.Error(t, current.snapshotToStore(context.Background()))

	current, client = newSnapshotSession()
	historyCalls := 0
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
		historyCalls++
		if historyCalls == 2 {
			return nil, errors.New("watermark failed")
		}

		return append([]opencode.SyncEvent(nil), client.syncEvents...), nil
	}
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "watermark failed")

	current, client = newSnapshotSession()
	originalWait := stateCaptureWait
	stateCaptureWait = func(context.Context, time.Duration) error { return nil }
	client.syncHistoryFunc = func(_ context.Context, cursors map[string]int64) ([]opencode.SyncEvent, error) {
		if len(cursors) > 0 {
			return []opencode.SyncEvent{syncTestEvent("native-1", 1, "session.updated.1", nil)}, nil
		}

		return append([]opencode.SyncEvent(nil), client.syncEvents...), nil
	}
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "changed during export")
	stateCaptureWait = originalWait

	current, _ = newSnapshotSession()
	originalRead := restoreRandRead
	restoreRandRead = func([]byte) (int, error) { return 0, errors.New("generation failed") }
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "generation failed")
	restoreRandRead = originalRead
	t.Cleanup(func() { restoreRandRead = originalRead })

	current, client = newSnapshotSession()
	client.syncEvents[0].Data["info"] = json.RawMessage(`{"metadata":"not-an-object"}`)
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "sanitize sync event")

	current, client = newSnapshotSession()
	client.syncEvents[0].Type = "message.part.updated.1"
	delete(client.syncEvents[0].Data, "info")
	client.syncEvents[0].Data["part"] = json.RawMessage(`{`)
	require.Error(t, current.snapshotToStore(context.Background()))

	current, client = newSnapshotSession()
	current.secretNeedles = []string{"credential-value"}
	client.syncEvents[0].Data["info"] = json.RawMessage(`{"id":"native-1","note":"credential-value"}`)
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "MCP credential")

	current, client = newSnapshotSession()
	client.xdg.Root = ""
	require.ErrorContains(t, current.snapshotToStore(context.Background()), "state directory is empty")
}

func TestRestoreSyncStateRemainingValidationReplayVerificationAndOwnershipBranches(t *testing.T) {
	invalid := validSyncSnapshot("session", "native", absTestPath("source"))
	invalid.Format = "wrong"
	_, err := restoreSyncState(context.Background(), newFakeOpenCodeClient(t), invalid, "native", absTestPath("target"))
	require.Error(t, err)

	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	client := newFakeOpenCodeClient(t)
	client.syncReplayErr = errors.New("replay failed")
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "replay failed")

	client = newFakeOpenCodeClient(t)
	historyCalls := 0
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
		historyCalls++
		if historyCalls == 2 {
			return nil, errors.New("verify history failed")
		}

		return nil, nil
	}
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "verify history failed")

	client = newFakeOpenCodeClient(t)
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) { return nil, nil }
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "failed replay verification")

	client = newFakeOpenCodeClient(t)
	historyCalls = 0
	expected, err := rebaseSyncEvents(snapshot.Events["native"], absTestPath("source"), absTestPath("target"))
	require.NoError(t, err)
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
		historyCalls++
		if historyCalls == 1 {
			return nil, nil
		}
		wrong := restoreOwnershipFile{Format: "opencode-restore-ownership-v1", Aggregates: map[string]restoreOwnership{}}
		encoded, marshalErr := json.Marshal(wrong)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName), encoded, 0o600))

		return expected, nil
	}
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", absTestPath("target"))
	require.ErrorContains(t, err, "lost durable restore ownership")
}

func TestRebaseArrayErrorAndSyncOrderComparison(t *testing.T) {
	_, err := rebasePathValues([]any{absTestPath("outside")}, "path", absTestPath("source"), absTestPath("target"))
	require.ErrorContains(t, err, "escapes source cwd")

	zero := syncTestEvent("native", 0, "session.created.1", nil)
	one := syncTestEvent("native", 1, "session.updated.1", nil)
	require.False(t, syncEventsEqual([]opencode.SyncEvent{zero, one}, []opencode.SyncEvent{one, zero}))
	require.True(t, syncEventsEqual([]opencode.SyncEvent{zero, zero}, []opencode.SyncEvent{zero, zero}))
}

func cloneStateSnapshot(t *testing.T, value stateSnapshot) stateSnapshot {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var cloned stateSnapshot
	require.NoError(t, json.Unmarshal(encoded, &cloned))

	return cloned
}

func TestCaptureStateSnapshotArtifactReplacementError(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.getSession = testNativeSession("native-1")
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(t, agent, client)
	session.cwd = t.TempDir()

	png := fixtureImage(t, "valid.png")
	require.NoError(t, session.registerImageArtifact(context.Background(), "id-1", imageArtifactRecord{
		Version: imageArtifactRecordVersion, Fingerprint: imageFingerprint(png), Mime: mimePNG,
		Data: base64.StdEncoding.EncodeToString(png), CreatedAtUnixMilli: imageArtifactNow().UnixMilli(),
	}))

	original := imageJSONMarshal
	imageJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }
	t.Cleanup(func() { imageJSONMarshal = original })

	require.Error(t, session.snapshotToStore(context.Background()))
}

func TestCaptureStateSnapshotRetriesUnstableSyncGeneration(t *testing.T) {
	newSnapshotSession := func() (*session, *fakeOpenCodeClient) {
		agent := NewAgent()
		client := newFakeOpenCodeClient(t)
		current := testSession(t, agent, client)
		agent.sessions[current.id] = current

		return current, client
	}

	// unstableHistory reports an allowlisted native write on the watermark read
	// of the first trips capture attempts, then goes quiet.
	unstableHistory := func(client *fakeOpenCodeClient, trips int, attempts *int) func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
		return func(_ context.Context, cursors map[string]int64) ([]opencode.SyncEvent, error) {
			if len(cursors) == 0 {
				*attempts++

				return append([]opencode.SyncEvent(nil), client.syncEvents...), nil
			}

			if *attempts <= trips {
				return []opencode.SyncEvent{syncTestEvent("native-1", 1, "session.updated.1", nil)}, nil
			}

			return nil, nil
		}
	}

	recordWaits := func(t *testing.T) *[]time.Duration {
		t.Helper()

		waits := &[]time.Duration{}
		original := stateCaptureWait
		stateCaptureWait = func(_ context.Context, delay time.Duration) error {
			*waits = append(*waits, delay)

			return nil
		}

		t.Cleanup(func() { stateCaptureWait = original })

		return waits
	}

	t.Run("retries past a write that lands between the two reads", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = unstableHistory(client, 1, &attempts)
		waits := recordWaits(t)

		require.NoError(t, current.snapshotToStore(context.Background()))
		require.Equal(t, 2, attempts)
		require.Equal(t, []time.Duration{stateCaptureBackoffBase}, *waits)
	})

	t.Run("exhausts the bounded schedule and reports the change", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = unstableHistory(client, stateCaptureAttempts, &attempts)
		waits := recordWaits(t)

		err := current.snapshotToStore(context.Background())
		require.ErrorIs(t, err, errGraphChangedDuringExport)
		require.EqualError(t, err, "OpenCode graph changed during export")
		require.Equal(t, stateCaptureAttempts, attempts)
		require.Equal(t, []time.Duration{
			50 * time.Millisecond,
			100 * time.Millisecond,
			200 * time.Millisecond,
			400 * time.Millisecond,
			800 * time.Millisecond,
		}, *waits)
	})

	t.Run("stops retrying when the settle budget is spent", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = unstableHistory(client, stateCaptureAttempts, &attempts)
		waits := recordWaits(t)

		originalBudget := stateCaptureSettleBudget
		stateCaptureSettleBudget = 0

		t.Cleanup(func() { stateCaptureSettleBudget = originalBudget })

		require.ErrorIs(t, current.snapshotToStore(context.Background()), errGraphChangedDuringExport)
		require.Equal(t, 1, attempts)
		require.Empty(t, *waits)
	})

	t.Run("keeps a caller deadline shorter than the settle budget", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = unstableHistory(client, stateCaptureAttempts, &attempts)
		waits := recordWaits(t)

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()

		require.ErrorIs(t, current.snapshotToStore(ctx), errGraphChangedDuringExport)
		require.Equal(t, 1, attempts)
		require.Empty(t, *waits)
	})

	t.Run("surfaces a cancelled backoff", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = unstableHistory(client, stateCaptureAttempts, &attempts)

		original := stateCaptureWait
		stateCaptureWait = func(context.Context, time.Duration) error { return context.Canceled }

		t.Cleanup(func() { stateCaptureWait = original })

		require.ErrorIs(t, current.snapshotToStore(context.Background()), context.Canceled)
		require.Equal(t, 1, attempts)
	})

	t.Run("never retries a durable capture defect", func(t *testing.T) {
		current, client := newSnapshotSession()
		attempts := 0
		client.syncHistoryFunc = func(_ context.Context, cursors map[string]int64) ([]opencode.SyncEvent, error) {
			if len(cursors) == 0 {
				attempts++

				return append([]opencode.SyncEvent(nil), client.syncEvents...), nil
			}

			return nil, errors.New("watermark failed")
		}
		waits := recordWaits(t)

		require.ErrorContains(t, current.snapshotToStore(context.Background()), "watermark failed")
		require.Equal(t, 1, attempts)
		require.Empty(t, *waits)
	})
}

func TestStateCaptureBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	require.Equal(t, []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}, []time.Duration{
		stateCaptureBackoff(0),
		stateCaptureBackoff(1),
		stateCaptureBackoff(2),
		stateCaptureBackoff(3),
		stateCaptureBackoff(4),
	})

	require.Equal(t, stateCaptureBackoffCap, stateCaptureBackoff(5))
	require.Equal(t, stateCaptureBackoffCap, stateCaptureBackoff(62))
}

func TestStateCaptureWaitHonoursDelayAndCancellation(t *testing.T) {
	require.NoError(t, stateCaptureWait(context.Background(), time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, stateCaptureWait(ctx, time.Hour), context.Canceled)
}
func TestStateSnapshotDecoderReachableEdges(t *testing.T) {
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	var original map[string]any
	require.NoError(t, json.Unmarshal(encoded, &original))
	encode := func(t *testing.T, mutate func(map[string]any)) []byte {
		t.Helper()
		candidate := cloneJSONMap(t, original)
		mutate(candidate)
		raw, marshalErr := json.Marshal(candidate)
		require.NoError(t, marshalErr)

		return raw
	}

	for name, raw := range map[string][]byte{
		"typed unmarshal": encode(t, func(value map[string]any) {
			value[snapshotFieldCapturedAtUnixMilli] = "not-an-integer"
		}),
		"environment object": encode(t, func(value map[string]any) {
			session, ok := value[snapshotFieldSession].(map[string]any)
			require.True(t, ok)
			session[snapshotFieldEnv] = []any{}
		}),
		"graph array": encode(t, func(value map[string]any) {
			value[snapshotFieldGraph] = map[string]any{}
		}),
		"events object": encode(t, func(value map[string]any) {
			value[jsonFieldEvents] = []any{}
		}),
		"aggregate array": encode(t, func(value map[string]any) {
			events, ok := value[jsonFieldEvents].(map[string]any)
			require.True(t, ok)
			events["native"] = map[string]any{}
		}),
		"event data object": encode(t, func(value map[string]any) {
			events, ok := value[jsonFieldEvents].(map[string]any)
			require.True(t, ok)
			aggregate, ok := events["native"].([]any)
			require.True(t, ok)
			event, ok := aggregate[0].(map[string]any)
			require.True(t, ok)
			event[jsonFieldData] = []any{}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			_, decodeErr := decodeStateSnapshot(raw)
			require.Error(t, decodeErr)
		})
	}

	require.Error(t, rejectDuplicateJSONFields([]byte(`[] {`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`{"x":1,"x":2}`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`{"x":1]`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`[1}`)))
	require.NoError(t, rejectDuplicateJSONFields([]byte(`{"x":[{"y":1}]}`)))
	require.Error(t, scanUniqueJSONValue(json.NewDecoder(bytes.NewReader(nil)), "empty"))
	require.Error(t, scanUniqueJSONValue(json.NewDecoder(bytes.NewReader([]byte(`[1`))), "array"))
	_, err = exactJSONObject([]byte(`{`), "invalid", nil, nil)
	require.Error(t, err)
	_, err = exactJSONObject([]byte(`null`), "null", nil, nil)
	require.Error(t, err)
}

func TestStateStoreValidationReachableEdges(t *testing.T) {
	node := stateSnapshotNode{SessionID: "session", NativeSessionID: "native", SourceCwd: absTestPath("source")}
	partEvent := opencode.SyncEvent{
		ID: "part", AggregateID: "native", Type: syncTypeMessagePartUpdated,
		Data: map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`),
			syncFieldPart:      json.RawMessage(`{}`),
			jsonFieldTime:      json.RawMessage(`{`),
		},
	}
	require.Error(t, validateSyncEvent(partEvent, node))
	partEvent.Data[jsonFieldTime] = json.RawMessage(`"one"`)
	require.Error(t, validateSyncEvent(partEvent, node))
	partEvent.Data[jsonFieldTime] = json.RawMessage(`1e10000`)
	require.Error(t, validateSyncEvent(partEvent, node))

	invalid := validSyncSnapshot("session", "native", absTestPath("source"))
	invalid.Format = "unsupported"
	entry, err := json.Marshal(invalid)
	require.NoError(t, err)
	store := NewInMemorySessionStore()
	require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{entry},
	}}))
	_, _, found, err := hydrateStateFromStore(t.Context(), store, "session")
	require.Error(t, err)
	require.False(t, found)

	badMarshal := validSyncSnapshot("session", "native", absTestPath("source"))
	badMarshal.Events["native"][0].Data[syncFieldInfo] = json.RawMessage(`{`)
	require.Error(t, scanStateSnapshot(badMarshal, nil))

	terminal := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "assistant", "assistant", "stop", int64Pointer(100)),
	)
	terminal = mutateTerminalTestSnapshot(t, terminal, func(value *stateSnapshot) {
		value.Events["native"][1].Data[syncFieldInfo] = json.RawMessage(
			`{"id":[],"sessionID":"native","role":"assistant","finish":"stop"}`,
		)
	})
	_, err = InspectSessionStoreTerminalState("session", []SessionStoreEntry{terminal})
	require.ErrorContains(t, err, "decode OpenCode message event")
}

// TestForkLineageRestoresInEveryCloseOrder is the fixture the fork bugs needed.
// A forked lineage must be restorable whichever member is closed first, so both
// orders are driven end to end: close, then resume and load every member that
// was closed.
func TestForkLineageRestoresInEveryCloseOrder(t *testing.T) {
	orders := map[string]bool{"parent first": true, "fork first": false}

	for name, parentFirst := range orders {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			agent, _ := forkLineageAgent(t)
			cwd := t.TempDir()

			parent, err := agent.NewSession(ctx, NewSessionRequest(cwd))
			require.NoError(t, err)

			fork, err := agent.forkSession(ctx, ForkSessionRequest(parent.SessionId, cwd))
			require.NoError(t, err)

			first, second := parent.SessionId, fork.SessionId
			if !parentFirst {
				first, second = fork.SessionId, parent.SessionId
			}

			_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first})
			require.NoError(t, err)
			_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: second})
			require.NoError(t, err)

			// Every member restores, in either direction, through both restore
			// surfaces.
			for _, id := range []acp.SessionId{parent.SessionId, fork.SessionId} {
				resumed, resumeErr := agent.ResumeSession(ctx, ResumeSessionRequest(id, cwd))
				require.NoError(t, resumeErr, "resume %s", id)
				require.NotNil(t, resumed.Meta)

				_, closeErr := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: id})
				require.NoError(t, closeErr)

				loaded, loadErr := agent.LoadSession(ctx, LoadSessionRequest(id, cwd))
				require.NoError(t, loadErr, "load %s", id)
				require.NotNil(t, loaded.Meta)
			}
		})
	}
}

// TestForkBundleCarriesItsOwnAggregateOnly pins the store-format fact a
// downstream reader depends on: one `main` entry per logical session, whose
// `graph` holds that session's node alone and whose `events` map holds that
// session's aggregate alone. The fork's node still names the lineage it came
// from.
func TestForkBundleCarriesItsOwnAggregateOnly(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	agent, _ := forkLineageAgent(t)
	agent.options.SessionStore = store
	cwd := t.TempDir()

	parent, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)

	fork, err := agent.forkSession(ctx, ForkSessionRequest(parent.SessionId, cwd))
	require.NoError(t, err)

	parentBundle := requireStoredSnapshot(t, store, string(parent.SessionId))
	require.Len(t, parentBundle.Graph, 1)
	require.Equal(t, string(parent.SessionId), parentBundle.Graph[0].SessionID)
	require.Empty(t, parentBundle.Graph[0].ParentSessionID)
	require.Equal(t, cwd, parentBundle.Graph[0].SourceCwd)
	require.Equal(t, []string{"native-parent"}, aggregateIDs(parentBundle))

	forkBundle := requireStoredSnapshot(t, store, string(fork.SessionId))
	require.Len(t, forkBundle.Graph, 1)
	require.Equal(t, string(fork.SessionId), forkBundle.Graph[0].SessionID)
	require.Equal(t, string(parent.SessionId), forkBundle.Graph[0].ParentSessionID)
	require.Equal(t, "native-parent", forkBundle.Graph[0].NativeParentID)
	require.Equal(t, cwd, forkBundle.Graph[0].SourceCwd)
	require.Equal(t, []string{"native-fork"}, aggregateIDs(forkBundle))
}

func requireStoredSnapshot(t *testing.T, store SessionStore, sessionID string) stateSnapshot {
	t.Helper()

	entries, err := store.Load(context.Background(), SessionKey{SessionID: sessionID, Subpath: SessionStoreMainSubpath})
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	snapshot, err := decodeStateSnapshot(entries[len(entries)-1])
	require.NoError(t, err)

	return snapshot
}

func aggregateIDs(snapshot stateSnapshot) []string {
	ids := make([]string, 0, len(snapshot.Events))
	for id := range snapshot.Events {
		ids = append(ids, id)
	}

	return ids
}
