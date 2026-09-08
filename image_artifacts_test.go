package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestImageArtifactStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)

	require.Equal(t, "images/"+fingerprint, imageArtifactSubpath(fingerprint))

	record := imageArtifactRecord{
		Version:            imageArtifactRecordVersion,
		NativeID:           "native-id",
		Provenance:         provenanceTool,
		Fingerprint:        fingerprint,
		Mime:               mimePNG,
		DataURLPrefix:      "data:image/png;base64,",
		Data:               base64.StdEncoding.EncodeToString(png),
		CreatedAtUnixMilli: imageArtifactNow().UnixMilli(),
	}
	require.Equal(t, "data:image/png;base64,"+record.Data, record.nativeDataURL())

	t.Run("register, lookup, clone", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		require.NoError(t, sess.registerImageArtifact(ctx, "id-1", record))
		// Identical bytes under a second identity register once.
		require.NoError(t, sess.registerImageArtifact(ctx, "id-2", record))

		got, ok := sess.imageArtifactByIdentity("id-1")
		require.True(t, ok)
		require.Equal(t, fingerprint, got.Fingerprint)

		_, ok = sess.imageArtifactByIdentity("unknown")
		require.False(t, ok)

		cloned := sess.cloneImageArtifacts()
		require.Len(t, cloned, 1)
	})

	t.Run("setImageArtifacts rebuilds identity index", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		sess.setImageArtifacts(map[string]imageArtifactRecord{fingerprint: record})
		got, ok := sess.imageArtifactByIdentity("native-id")
		require.True(t, ok)
		require.Equal(t, fingerprint, got.Fingerprint)
	})

	t.Run("store append failure is storage_failed", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		sess.agent.options.SessionStore = &errorSessionStore{err: errors.New("append boom")}
		err := sess.registerImageArtifact(ctx, "id-1", record)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
		_, exists := sess.imageArtifactByIdentity("id-1")
		require.False(t, exists, "failed persistence published an artifact identity")
		require.Empty(t, sess.cloneImageArtifacts())

		store := NewInMemorySessionStore()
		sess.agent.options.SessionStore = store
		require.NoError(t, sess.registerImageArtifact(ctx, "id-1", record))
		entries, err := store.Load(ctx, SessionKey{SessionID: string(sess.id), Subpath: imageArtifactSubpath(fingerprint)})
		require.NoError(t, err)
		require.Len(t, entries, 1, "retry skipped the durable artifact write")
	})

	t.Run("marshal failure is storage_failed", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		original := imageJSONMarshal
		imageJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }
		t.Cleanup(func() { imageJSONMarshal = original })
		err := sess.registerImageArtifact(ctx, "id-1", record)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})
}

func TestLoadSessionImageArtifacts(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)

	newRecord := func() imageArtifactRecord {
		return imageArtifactRecord{
			Version:            imageArtifactRecordVersion,
			Fingerprint:        fingerprint,
			Mime:               mimePNG,
			Data:               base64.StdEncoding.EncodeToString(png),
			CreatedAtUnixMilli: imageArtifactNow().UnixMilli(),
		}
	}

	t.Run("loads a live record", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		require.NoError(t, sess.registerImageArtifact(ctx, "id-1", newRecord()))
		loaded, err := sess.agent.loadSessionImageArtifacts(ctx, string(sess.id))
		require.NoError(t, err)
		require.Len(t, loaded, 1)
	})

	t.Run("sweeps an expired record", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		require.NoError(t, sess.registerImageArtifact(ctx, "id-1", newRecord()))

		original := imageArtifactNow
		imageArtifactNow = func() time.Time { return original().Add(imageArtifactTTL + time.Hour) }
		t.Cleanup(func() { imageArtifactNow = original })

		loaded, err := sess.agent.loadSessionImageArtifacts(ctx, string(sess.id))
		require.NoError(t, err)
		require.Empty(t, loaded)

		subkeys, err := sess.agent.sessionStore().ListSubkeys(ctx, SessionKey{SessionID: string(sess.id)})
		require.NoError(t, err)
		require.NotContains(t, subkeys, imageArtifactSubpath(fingerprint))
	})

	t.Run("corrupt record fails closed", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		key := SessionKey{SessionID: string(sess.id), Subpath: imageArtifactSubpath("deadbeef")}
		require.NoError(t, sess.agent.sessionStore().Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{`)}))
		_, err := sess.agent.loadSessionImageArtifacts(ctx, string(sess.id))
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})

	t.Run("list failure propagates", func(t *testing.T) {
		sess := testSession(t, NewAgent(), newFakeOpenCodeClient(t))
		sess.agent.options.SessionStore = &errorSessionStore{err: errors.New("list boom")}
		_, err := sess.agent.loadSessionImageArtifacts(ctx, string(sess.id))
		require.Error(t, err)
	})
}

func TestLoadSessionImageArtifactsStoreFaults(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)
	sessionID := "session-1"
	record := imageArtifactRecord{
		Version: imageArtifactRecordVersion, Fingerprint: fingerprint, Mime: mimePNG,
		Data: base64.StdEncoding.EncodeToString(png), CreatedAtUnixMilli: imageArtifactNow().UnixMilli(),
	}
	entry, err := json.Marshal(record)
	require.NoError(t, err)

	newAgentWithStore := func(store SessionStore) *Agent {
		agent := NewAgent()
		agent.options.SessionStore = store

		return agent
	}

	t.Run("non-image subkeys are skipped", func(t *testing.T) {
		store := NewInMemorySessionStore()
		require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: imageArtifactSubpath(fingerprint)}, []SessionStoreEntry{entry}))
		require.NoError(t, store.Append(ctx, SessionKey{SessionID: sessionID, Subpath: "other/thing"}, []SessionStoreEntry{json.RawMessage(`1`)}))
		loaded, err := newAgentWithStore(store).loadSessionImageArtifacts(ctx, sessionID)
		require.NoError(t, err)
		require.Len(t, loaded, 1)
	})

	t.Run("load failure propagates", func(t *testing.T) {
		store := &hookSessionStore{
			InMemorySessionStore: NewInMemorySessionStore(),
			onList:               func() ([]string, error) { return []string{imageArtifactSubpath(fingerprint)}, nil },
			onLoad:               func(SessionKey) ([]SessionStoreEntry, error) { return nil, errors.New("load boom") },
		}
		_, err := newAgentWithStore(store).loadSessionImageArtifacts(ctx, sessionID)
		require.Error(t, err)
	})

	t.Run("empty entries are skipped", func(t *testing.T) {
		store := &hookSessionStore{
			InMemorySessionStore: NewInMemorySessionStore(),
			onList:               func() ([]string, error) { return []string{imageArtifactSubpath(fingerprint)}, nil },
			onLoad:               func(SessionKey) ([]SessionStoreEntry, error) { return nil, nil },
		}
		loaded, err := newAgentWithStore(store).loadSessionImageArtifacts(ctx, sessionID)
		require.NoError(t, err)
		require.Empty(t, loaded)
	})

	t.Run("sweep delete failure propagates", func(t *testing.T) {
		store := &hookSessionStore{
			InMemorySessionStore: NewInMemorySessionStore(),
			onList:               func() ([]string, error) { return []string{imageArtifactSubpath(fingerprint)}, nil },
			onLoad:               func(SessionKey) ([]SessionStoreEntry, error) { return []SessionStoreEntry{entry}, nil },
			onDelete:             func(SessionKey) error { return errors.New("delete boom") },
		}
		original := imageArtifactNow
		imageArtifactNow = func() time.Time { return original().Add(imageArtifactTTL + time.Hour) }
		t.Cleanup(func() { imageArtifactNow = original })
		_, err := newAgentWithStore(store).loadSessionImageArtifacts(ctx, sessionID)
		require.Error(t, err)
	})
}

func TestDecodeImageArtifactRecordAndStoreFailure(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)
	valid := imageArtifactRecord{
		Version:     imageArtifactRecordVersion,
		Fingerprint: fingerprint,
		Mime:        mimePNG,
		Data:        base64.StdEncoding.EncodeToString(png),
	}

	entry := func(record imageArtifactRecord) SessionStoreEntry {
		raw, err := json.Marshal(record)
		require.NoError(t, err)

		return raw
	}

	t.Run("valid", func(t *testing.T) {
		got, err := decodeImageArtifactRecord(entry(valid), fingerprint)
		require.NoError(t, err)
		require.Equal(t, fingerprint, got.Fingerprint)
	})

	t.Run("bad json", func(t *testing.T) {
		_, err := decodeImageArtifactRecord(json.RawMessage(`{`), fingerprint)
		require.Error(t, err)
	})

	t.Run("wrong version", func(t *testing.T) {
		bad := valid
		bad.Version = 99
		_, err := decodeImageArtifactRecord(entry(bad), fingerprint)
		require.Error(t, err)
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		_, err := decodeImageArtifactRecord(entry(valid), "other")
		require.Error(t, err)
	})

	t.Run("cancellation is not a storage failure", func(t *testing.T) {
		require.ErrorIs(t, imageStoreFailure("op", context.Canceled), context.Canceled)
		data := assertTurnFailed(t, imageStoreFailure("op", errors.New("boom")), causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})
}

func TestSyncEventImageSanitizeAndRehydrate(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)
	prefix := "data:image/png;base64,"
	payload := base64.StdEncoding.EncodeToString(png)
	record := imageArtifactRecord{Fingerprint: fingerprint, Mime: mimePNG, DataURLPrefix: prefix, Data: payload}
	artifacts := map[string]imageArtifactRecord{fingerprint: record}

	buildEvents := func(url string) map[string][]opencode.SyncEvent {
		return map[string][]opencode.SyncEvent{"agg": {{
			ID: "e1", AggregateID: "agg", Data: map[string]json.RawMessage{
				syncFieldPart: json.RawMessage(`{"url":"` + url + `"}`),
			},
		}}}
	}

	t.Run("round trip", func(t *testing.T) {
		events := buildEvents(record.nativeDataURL())
		sanitizeSyncEventImages(events, artifacts)
		require.Contains(t, string(events["agg"][0].Data[syncFieldPart]), imageArtifactReferenceScheme+fingerprint)
		require.NotContains(t, string(events["agg"][0].Data[syncFieldPart]), payload)

		require.NoError(t, rehydrateSyncEventImages(events, artifacts))
		require.Contains(t, string(events["agg"][0].Data[syncFieldPart]), payload)
	})

	t.Run("empty artifacts is a no-op", func(t *testing.T) {
		events := buildEvents(record.nativeDataURL())
		sanitizeSyncEventImages(events, nil)
		require.Contains(t, string(events["agg"][0].Data[syncFieldPart]), payload)
	})

	t.Run("no data-url artifacts is a no-op", func(t *testing.T) {
		events := buildEvents(record.nativeDataURL())
		sanitizeSyncEventImages(events, map[string]imageArtifactRecord{"x": {Fingerprint: "x"}})
		require.Contains(t, string(events["agg"][0].Data[syncFieldPart]), payload)
	})

	t.Run("unmatched data url stays verbatim", func(t *testing.T) {
		events := buildEvents("data:image/png;base64,QUJD")
		sanitizeSyncEventImages(events, artifacts)
		require.Contains(t, string(events["agg"][0].Data[syncFieldPart]), "QUJD")
	})

	t.Run("rehydrate missing artifact fails", func(t *testing.T) {
		events := map[string][]opencode.SyncEvent{"agg": {{
			ID: "e1", AggregateID: "agg", Data: map[string]json.RawMessage{
				syncFieldPart: json.RawMessage(`{"url":"` + imageArtifactReferenceScheme + "missing" + `"}`),
			},
		}}}
		data := assertTurnFailed(t, rehydrateSyncEventImages(events, artifacts), causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})

	t.Run("events without a part are skipped", func(t *testing.T) {
		events := map[string][]opencode.SyncEvent{"agg": {{ID: "e1", AggregateID: "agg", Data: map[string]json.RawMessage{}}}}
		sanitizeSyncEventImages(events, artifacts)
		require.NoError(t, rehydrateSyncEventImages(events, artifacts))
	})

	t.Run("replacements marshal every artifact", func(t *testing.T) {
		replacements, err := imageArtifactReplacements("session-1", artifacts)
		require.NoError(t, err)
		require.Len(t, replacements, 1)

		original := imageJSONMarshal
		imageJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }
		t.Cleanup(func() { imageJSONMarshal = original })
		_, err = imageArtifactReplacements("session-1", artifacts)
		require.Error(t, err)
	})
}

func TestDataURLByteScanEdges(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	fingerprint := imageFingerprint(png)
	prefix := "data:image/png;base64,"
	payload := base64.StdEncoding.EncodeToString(png)
	byURL := map[string]string{prefix + payload: fingerprint}

	t.Run("escaped candidate is not replaced", func(t *testing.T) {
		raw := json.RawMessage(`"` + prefix + `AB\\C"`)
		require.Equal(t, raw, replaceDataURLs(raw, byURL))
	})

	t.Run("unterminated data marker breaks", func(t *testing.T) {
		raw := json.RawMessage(`"` + prefix + `AA`)
		require.Equal(t, raw, replaceDataURLs(raw, byURL))
	})

	t.Run("restore without marker returns input", func(t *testing.T) {
		raw := json.RawMessage(`{"text":"plain"}`)
		restored, err := restoreDataURLs(raw, map[string]imageArtifactRecord{})
		require.NoError(t, err)
		require.Equal(t, raw, restored)
	})

	t.Run("restore with unterminated marker breaks", func(t *testing.T) {
		raw := json.RawMessage(`"` + imageArtifactReferenceScheme + fingerprint)
		restored, err := restoreDataURLs(raw, map[string]imageArtifactRecord{})
		require.NoError(t, err)
		require.Equal(t, raw, restored)
	})
}

func TestLoadAndRehydrateArtifacts(t *testing.T) {
	ctx := context.Background()

	t.Run("load error propagates", func(t *testing.T) {
		store := NewInMemorySessionStore()
		require.NoError(t, store.Append(ctx, SessionKey{SessionID: "s", Subpath: imageArtifactSubpath("x")}, []SessionStoreEntry{json.RawMessage(`{`)}))
		agent := NewAgent()
		agent.options.SessionStore = store
		_, err := agent.loadAndRehydrateArtifacts(ctx, "s", nil)
		require.Error(t, err)
	})

	t.Run("rehydrate error propagates", func(t *testing.T) {
		agent := NewAgent()
		events := map[string][]opencode.SyncEvent{"agg": {{
			ID: "e", AggregateID: "agg", Data: map[string]json.RawMessage{
				syncFieldPart: json.RawMessage(`{"url":"` + imageArtifactReferenceScheme + `missing"}`),
			},
		}}}
		_, err := agent.loadAndRehydrateArtifacts(ctx, "s", events)
		require.Error(t, err)
	})

	t.Run("success returns loaded artifacts", func(t *testing.T) {
		agent := NewAgent()
		got, err := agent.loadAndRehydrateArtifacts(ctx, "s", nil)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}
