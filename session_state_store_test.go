package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"errors"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func syncTestEvent(aggregate string, sequence int64, kind string, extra map[string]json.RawMessage) opencode.SyncEvent {
	data := map[string]json.RawMessage{
		"sessionID": json.RawMessage(`"` + aggregate + `"`),
		"info":      json.RawMessage(`{"id":"` + aggregate + `","directory":"/source"}`),
	}
	for key, value := range extra {
		data[key] = value
	}

	return opencode.SyncEvent{ID: aggregate + "-evt", AggregateID: aggregate, Sequence: sequence, Type: kind, Data: data}
}

func TestAllowlistedSyncEventsRejectsCrossAggregateAndUnknownSchema(t *testing.T) {
	allow := map[string]stateSnapshotNode{"a": {NativeSessionID: "a", SourceCwd: "/source"}}
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

func TestSyncSnapshotHardRejectsOldAndIncompleteFormats(t *testing.T) {
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(context.Background(), SessionKey{SessionID: "s"}, []SessionStoreEntry{
		json.RawMessage(`{"format":"removed-format"}`),
	}))
	idmap, hydrated, found, err := hydrateStateFromStore(context.Background(), store, "s")
	require.Empty(t, idmap)
	require.Empty(t, hydrated)
	require.False(t, found)
	require.ErrorContains(t, err, "unsupported opencode store format")

	snapshot := validSyncSnapshot("s", "native", "/source")
	delete(snapshot.Events, "native")
	require.ErrorContains(t, validateSyncSnapshot("s", snapshot), "incomplete")
}

func TestRestoreRebasesAndVerifiesExactEventSet(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	snapshot := validSyncSnapshot("s", "native", "/source")
	native, err := restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.NoError(t, err)
	require.Equal(t, "native", native.ID)
	require.Len(t, client.syncEvents, 1)
	var info map[string]any
	require.NoError(t, json.Unmarshal(client.syncEvents[0].Data["info"], &info))
	require.Equal(t, "/target", info["directory"])
	_, err = os.Stat(filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName))
	require.NoError(t, err)

	// A complete retry is idempotent and verifies rather than duplicating.
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 1)

	client.syncEvents[0].Data["info"] = json.RawMessage(`{"id":"foreign","directory":"/target"}`)
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "owned by another restore")
}

func TestRestoreComparesExistingNativeCarrierThroughPortableProjection(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	snapshot := validSyncSnapshot("s", "native", "/source")

	nativeEvent := cloneSyncEvent(snapshot.Events["native"][0])
	nativeEvent.Data[syncFieldInfo] = json.RawMessage(`{
		"id":"native",
		"directory":"/source",
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

	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/source")
	require.NoError(t, err)
	require.Len(t, client.syncEvents, 1, "matching native history must be verified, not replayed")
	require.Contains(t, string(client.syncEvents[0].Data[syncFieldInfo]), "stale-reference",
		"portable comparison must not mutate the live native event")
}

func TestRestoreRejectsMalformedCarrierInExistingAndVerifiedNativeHistory(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		snapshot := validSyncSnapshot("s", "native", "/source")
		require.NoError(t, recordSnapshotOwnership(client, snapshot))
		malformed := cloneSyncEvent(snapshot.Events["native"][0])
		malformed.Data[syncFieldInfo] = json.RawMessage(`{"metadata":"not-an-object"}`)
		client.syncEvents = []opencode.SyncEvent{malformed}

		_, err := restoreSyncState(context.Background(), client, snapshot, "native", "/source")
		require.ErrorContains(t, err, "sanitize sync event")
	})

	t.Run("verified after replay", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		snapshot := validSyncSnapshot("s", "native", "/source")
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

		_, err := restoreSyncState(context.Background(), client, snapshot, "native", "/source")
		require.ErrorContains(t, err, "sanitize sync event")
	})
}

func TestRestoreRejectsExistingAggregateWithoutDurableOwner(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.syncEvents = []opencode.SyncEvent{syncTestEvent("native", 0, "session.created.1", nil)}
	snapshot := validSyncSnapshot("s", "native", "/source")

	_, err := restoreSyncState(context.Background(), client, snapshot, "native", "/source")
	require.ErrorContains(t, err, "no durable restore owner")
}

func TestRestoreRejectsPathOutsideCapturedCWD(t *testing.T) {
	client := newFakeOpenCodeClient()
	snapshot := validSyncSnapshot("s", "native", "/source")
	snapshot.Events["native"][0].Data["info"] = json.RawMessage(`{"id":"native","directory":"/other"}`)

	_, err := restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "escapes source cwd")
	require.Empty(t, client.syncEvents)
}

func TestBundleCredentialScan(t *testing.T) {
	require.NoError(t, scanSyncBundle([]byte(`{"text":"ordinary"}`), nil))
	require.ErrorContains(t, scanSyncBundle([]byte(`{"authorization":"Bearer secret"}`), nil), "forbidden")
	require.ErrorContains(t, scanSyncBundle([]byte(`{"text":"Bearer exact"}`), []string{"Bearer exact"}), "MCP credential")
}

func TestReadSyncGenerationRemovesOnlyNativeSessionCarrierReference(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	client.syncEvents[0].Data[syncFieldInfo] = json.RawMessage(`{
		"id":"native-1",
		"directory":"/source",
		"metadata":{
			"native":{"kept":true},
			"acp-go-opencode":{"ref":"opaque-operation-reference"}
		}
	}`)

	events, err := current.readSyncGeneration(
		context.Background(),
		map[string]stateSnapshotNode{"native-1": {
			SessionID: string(current.id), NativeSessionID: "native-1", SourceCwd: "/source",
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
		Session: stateSnapshotSession{SessionID: sessionID, NativeSessionID: nativeID, Cwd: cwd, ExtraPathDirs: []string{}},
		Graph:   []stateSnapshotNode{{SessionID: sessionID, NativeSessionID: nativeID, SourceCwd: cwd, Permission: "ask"}},
		Events:  map[string][]opencode.SyncEvent{nativeID: {event}},
	}
}
func TestSyncSnapshotValidationEveryFailureShape(t *testing.T) {
	base := validSyncSnapshot("session", "native", "/source")
	require.NoError(t, validateSyncSnapshot("session", base))

	tests := map[string]func(*stateSnapshot){
		"format":               func(value *stateSnapshot) { value.Format = "old" },
		"event schema":         func(value *stateSnapshot) { value.EventSchemaVersion = "old" },
		"session identity":     func(value *stateSnapshot) { value.Session.SessionID = "other" },
		"native identity":      func(value *stateSnapshot) { value.Session.NativeSessionID = "" },
		"generation":           func(value *stateSnapshot) { value.RestoreGeneration = "" },
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

func TestSyncEventAllowlistAndIdentityBranches(t *testing.T) {
	node := stateSnapshotNode{SessionID: "session", NativeSessionID: "native", SourceCwd: "/source"}
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

	snapshot := validSyncSnapshot("session", "native", "/source")
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
		"directory": "/source", "cwd": "/source/sub", "root": "relative", "other": "/source/ignored",
		"nested": []any{map[string]any{"path": "/source/file"}, true},
	}
	rebased, err := rebasePathValues(value, "", "/source", "/target")
	require.NoError(t, err)
	result, ok := rebased.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "/target", result["directory"])
	require.Equal(t, filepath.Join("/target", "sub"), result["cwd"])
	require.Equal(t, "relative", result["root"])
	require.Equal(t, "/source/ignored", result["other"])
	_, err = rebasePathValues("/outside", "path", "/source", "/target")
	require.ErrorContains(t, err, "escapes source cwd")

	badEvent := cloneSyncEvent(snapshot.Events["native"][0])
	badEvent.Data["info"] = json.RawMessage(`{`)
	_, err = rebaseSyncEvents([]opencode.SyncEvent{badEvent}, "/source", "/target")
	require.Error(t, err)
	samePathEvent := cloneSyncEvent(snapshot.Events["native"][0])
	samePathEvent.Data["info"] = json.RawMessage(`{ "directory": "/source", "id": "native" }`)
	samePath, err := rebaseSyncEvents([]opencode.SyncEvent{samePathEvent}, "/source/.", "/source")
	require.NoError(t, err)
	require.Equal(t, samePathEvent.Data["info"], samePath[0].Data["info"], "same-path restore must preserve native JSON bytes")

	event := snapshot.Events["native"][0]
	require.True(t, syncEventPrefix(nil, []opencode.SyncEvent{event}))
	require.False(t, syncEventPrefix([]opencode.SyncEvent{event, event}, []opencode.SyncEvent{event}))
	require.False(t, syncEventsEqual(nil, []opencode.SyncEvent{event}))
	changed := cloneSyncEvent(event)
	changed.ID = "changed"
	require.False(t, syncEventsEqual([]opencode.SyncEvent{event}, []opencode.SyncEvent{changed}))
	sequenceOne := cloneSyncEvent(event)
	sequenceOne.Sequence = 1
	require.True(t, syncEventsEqual([]opencode.SyncEvent{sequenceOne, event}, []opencode.SyncEvent{event, sequenceOne}))
}

func TestSnapshotBlockSecretsAndGenerationBranches(t *testing.T) {
	agent := NewAgent(WithEnv(map[string]string{
		"API_TOKEN": "token", "PASSWORD": "password", "COOKIE": "cookie", "EMPTY_TOKEN": "", "NORMAL": "ignored",
	}))
	client := newFakeOpenCodeClient()
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
		client := newFakeOpenCodeClient()
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
	invalid := validSyncSnapshot("session", "native", "/source")
	invalid.Format = "wrong"
	_, err := restoreSyncState(context.Background(), newFakeOpenCodeClient(), invalid, "native", "/target")
	require.Error(t, err)

	snapshot := validSyncSnapshot("session", "native", "/source")
	client := newFakeOpenCodeClient()
	client.syncReplayErr = errors.New("replay failed")
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "replay failed")

	client = newFakeOpenCodeClient()
	historyCalls := 0
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
		historyCalls++
		if historyCalls == 2 {
			return nil, errors.New("verify history failed")
		}

		return nil, nil
	}
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "verify history failed")

	client = newFakeOpenCodeClient()
	client.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) { return nil, nil }
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "failed exact replay verification")

	client = newFakeOpenCodeClient()
	historyCalls = 0
	expected, err := rebaseSyncEvents(snapshot.Events["native"], "/source", "/target")
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
	_, err = restoreSyncState(context.Background(), client, snapshot, "native", "/target")
	require.ErrorContains(t, err, "lost durable restore ownership")
}

func TestRebaseArrayErrorAndSyncSortComparatorBranches(t *testing.T) {
	_, err := rebasePathValues([]any{"/outside"}, "path", "/source", "/target")
	require.ErrorContains(t, err, "escapes source cwd")

	zero := syncTestEvent("native", 0, "session.created.1", nil)
	one := syncTestEvent("native", 1, "session.updated.1", nil)
	require.True(t, syncEventsEqual([]opencode.SyncEvent{zero, one}, []opencode.SyncEvent{one, zero}))
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
	client := newFakeOpenCodeClient()
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
		client := newFakeOpenCodeClient()
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
