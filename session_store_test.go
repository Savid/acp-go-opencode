package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInMemoryStoreReplaceTombstonesUnlistedSubpaths(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	main := SessionKey{SessionID: "s1", Subpath: SessionStoreMainSubpath}
	if err := store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{"format":"opencode-sync-events-v1"}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "idmap"}, Entries: []SessionStoreEntry{json.RawMessage(`{"sessionId":"s1"}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "old"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{"format":"opencode-sync-events-v1"}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "idmap"}, Entries: []SessionStoreEntry{json.RawMessage(`{"sessionId":"s1"}`)}},
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	subkeys, err := store.ListSubkeys(ctx, main)
	if err != nil {
		t.Fatalf("ListSubkeys: %v", err)
	}
	if len(subkeys) != 1 || subkeys[0] != "idmap" {
		t.Fatalf("subkeys = %#v", subkeys)
	}
	old, err := store.Load(ctx, SessionKey{SessionID: "s1", Subpath: "old"})
	if err != nil {
		t.Fatalf("Load old: %v", err)
	}
	if len(old) != 0 {
		t.Fatalf("old subpath visible: %#v", old)
	}
}

func TestInMemoryStoreAppendLoadDeleteListAndErrors(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	key := SessionKey{SessionID: "s1", Subpath: SessionStoreMainSubpath}
	subkey := SessionKey{SessionID: "s1", Subpath: "idmap"}
	var nilStore *InMemorySessionStore

	for name, fn := range map[string]func(context.Context) error{
		"append": func(ctx context.Context) error {
			return nilStore.Append(ctx, key, []SessionStoreEntry{json.RawMessage(`{}`)})
		},
		"load": func(ctx context.Context) error {
			_, err := nilStore.Load(ctx, key)

			return err
		},
		"replace": func(ctx context.Context) error {
			return nilStore.Replace(ctx, key, []SessionStoreReplacement{{Key: key, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}}})
		},
		"delete": func(ctx context.Context) error { return nilStore.Delete(ctx, key) },
		"list": func(ctx context.Context) error {
			_, err := nilStore.ListSessions(ctx)

			return err
		},
		"subkeys": func(ctx context.Context) error {
			_, err := nilStore.ListSubkeys(ctx, key)

			return err
		},
	} {
		t.Run(name+" canceled", func(t *testing.T) {
			if err := fn(cancelled); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled err = %v", err)
			}
		})
		t.Run(name+" nil", func(t *testing.T) {
			if err := fn(ctx); err == nil {
				t.Fatal("nil store call succeeded")
			}
		})
	}

	store := &InMemorySessionStore{}
	if err := store.Append(ctx, key, nil); err != nil {
		t.Fatalf("append empty: %v", err)
	}
	if err := store.Append(ctx, SessionKey{Subpath: "sub"}, []SessionStoreEntry{json.RawMessage(`{}`)}); err == nil ||
		!strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("empty session id append err = %v", err)
	}
	if err := store.Replace(ctx, SessionKey{}, []SessionStoreReplacement{{Key: SessionKey{}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}}}); err == nil ||
		!strings.Contains(err.Error(), "session id is required") {
		t.Fatalf("empty session id replace err = %v", err)
	}
	// Deleting an empty-SessionID key is a pure no-op: no error, no tombstone.
	if err := store.Delete(ctx, SessionKey{}); err != nil {
		t.Fatalf("empty session id delete: %v", err)
	}
	store.mu.Lock()
	if len(store.tombstones) != 0 {
		store.mu.Unlock()
		t.Fatalf("empty session id delete left tombstones: %#v", store.tombstones)
	}
	store.mu.Unlock()
	snapshot := validSyncSnapshot("s1", "native-1", absTestPath("repo"))
	snapshot.CapturedAtUnixMilli = 200
	snapshot.Session.Title = "Stored"
	entryBytes, err := json.Marshal(snapshot)
	require.NoError(t, err)
	entry := SessionStoreEntry(entryBytes)
	if appendErr := store.Append(ctx, key, []SessionStoreEntry{entry}); appendErr != nil {
		t.Fatalf("append main: %v", appendErr)
	}
	entry[0] = '['
	loaded, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(loaded[0]) == string(entry) {
		t.Fatal("store entry was not cloned")
	}
	loaded[0][0] = '['
	loadedAgain, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load again: %v", err)
	}
	if loadedAgain[0][0] == '[' {
		t.Fatal("loaded entry was not cloned")
	}
	if err = store.Append(ctx, subkey, []SessionStoreEntry{json.RawMessage(`{"sub":true}`)}); err != nil {
		t.Fatalf("append subkey: %v", err)
	}
	if err = store.Append(ctx, SessionKey{SessionID: "s0", Subpath: SessionStoreMainSubpath}, []SessionStoreEntry{json.RawMessage(`{bad}`)}); err != nil {
		t.Fatalf("append invalid summary: %v", err)
	}
	summaries, err := store.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var stored *SessionSummary
	for i := range summaries {
		if summaries[i].SessionID == "s1" {
			stored = &summaries[i]
		}
	}
	if len(summaries) != 2 || stored == nil || stored.Cwd != absTestPath("repo") || stored.Title != "Stored" {
		t.Fatalf("summaries = %#v", summaries)
	}
	subkeys, err := store.ListSubkeys(ctx, key)
	if err != nil {
		t.Fatalf("list subkeys: %v", err)
	}
	if len(subkeys) != 1 || subkeys[0] != "idmap" {
		t.Fatalf("subkeys = %#v", subkeys)
	}
	assertStoreTombstoneAndTieBreak(t, ctx, store, key, subkey)
}

func assertStoreTombstoneAndTieBreak(t *testing.T, ctx context.Context, store *InMemorySessionStore, key, subkey SessionKey) {
	t.Helper()
	if err := store.Delete(ctx, SessionKey{SessionID: "missing", Subpath: "sub"}); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := store.Delete(ctx, subkey); err != nil {
		t.Fatalf("delete subkey: %v", err)
	}
	if err := store.Append(ctx, subkey, []SessionStoreEntry{json.RawMessage(`{"ignored":true}`)}); err != nil {
		t.Fatalf("append tombstoned subkey: %v", err)
	}
	loadedSubkey, err := store.Load(ctx, subkey)
	if err != nil {
		t.Fatalf("load tombstoned subkey: %v", err)
	}
	if len(loadedSubkey) != 0 {
		t.Fatalf("tombstoned subkey loaded entries: %#v", loadedSubkey)
	}
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("delete main: %v", err)
	}
	if err = store.Append(ctx, SessionKey{SessionID: "s1", Subpath: "other"}, []SessionStoreEntry{json.RawMessage(`{"ignored":true}`)}); err != nil {
		t.Fatalf("append tombstoned main subkey: %v", err)
	}
	loadedMain, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("load tombstoned main: %v", err)
	}
	if len(loadedMain) != 0 {
		t.Fatalf("tombstoned main loaded entries: %#v", loadedMain)
	}

	tieStore := NewInMemorySessionStore()
	for _, id := range []string{"b", "a"} {
		if err = tieStore.Append(ctx, SessionKey{SessionID: id, Subpath: SessionStoreMainSubpath}, []SessionStoreEntry{json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("append tie %s: %v", id, err)
		}
	}
	tieStore.mu.Lock()
	tieStore.updatedAt[SessionKey{SessionID: "a", Subpath: SessionStoreMainSubpath}] = 1
	tieStore.updatedAt[SessionKey{SessionID: "b", Subpath: SessionStoreMainSubpath}] = 1
	tieStore.mu.Unlock()
	tied, err := tieStore.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list tied sessions: %v", err)
	}
	if len(tied) != 2 || tied[0].SessionID != "a" || tied[1].SessionID != "b" {
		t.Fatalf("tied summaries = %#v", tied)
	}
	if (&InMemorySessionStore{}).isTombstonedLocked(SessionKey{SessionID: "s"}) {
		t.Fatal("nil tombstones reported tombstoned")
	}
}

func TestInMemoryStoreReplaceValidation(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	main := SessionKey{SessionID: "s1", Subpath: SessionStoreMainSubpath}
	for name, replacements := range map[string][]SessionStoreReplacement{
		"missing main":  {{Key: SessionKey{SessionID: "s1", Subpath: "sub"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}}},
		"wrong session": {{Key: SessionKey{SessionID: "s2", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}}},
		"duplicate main": {
			{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
			{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Replace(ctx, main, replacements); err == nil {
				t.Fatal("replace unexpectedly succeeded")
			}
		})
	}
	// A refused duplicate names the key it refused, subpath included, so a caller
	// holding a long replacement set is not left to diff it by hand.
	require.ErrorContains(t, store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "idmap"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "idmap"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
	}), `duplicate replacement key: session "s1" subpath "idmap"`)

	require.ErrorContains(t, store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
	}), `duplicate replacement key: session "s1" subpath ""`)
	require.ErrorContains(t, store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		{Key: SessionKey{Subpath: "artifact"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
	}), "replacement session id is required")

	if err := store.Replace(ctx, SessionKey{}, nil); err == nil {
		t.Fatal("replace accepted missing main session id")
	}
	if err := store.Replace(ctx, SessionKey{SessionID: "s1", Subpath: "sub"}, nil); err == nil {
		t.Fatal("replace accepted non-main subpath")
	}
	if err := store.Replace(ctx, main, []SessionStoreReplacement{
		{Key: main, Entries: []SessionStoreEntry{json.RawMessage(`{"ok":true}`)}},
		{Key: SessionKey{SessionID: "s1", Subpath: "empty"}, Entries: nil},
	}); err != nil {
		t.Fatalf("replace with empty subkey: %v", err)
	}

	// A replacement leaves exactly the keys it lists alive: a listed key
	// survives even when its Entries are empty, so the empty subkey stays live
	// rather than being tombstoned.
	subkeys, err := store.ListSubkeys(ctx, main)
	if err != nil {
		t.Fatalf("list subkeys: %v", err)
	}

	found := false
	for _, subkey := range subkeys {
		if subkey == "empty" {
			found = true
		}
	}

	if !found {
		t.Fatalf("listed empty-entry subkey did not survive: %v", subkeys)
	}

	entries, err := store.Load(ctx, SessionKey{SessionID: "s1", Subpath: "empty"})
	if err != nil {
		t.Fatalf("load empty subkey: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("empty subkey entries = %v", entries)
	}
}

// TestInMemoryStoreReplaceIsOneSessionsAndRefusesBeforeWriting is the
// store-contract conformance fixture for the rule every host store owes: a
// `Replace` is one session's. A replacement key naming another session, and a
// `{SessionID, Subpath}` the set lists twice, are both refused with an error
// naming the offending key — and refused *before* anything is written, so a set
// the store will not accept leaves it byte-for-byte as it was.
func TestInMemoryStoreReplaceIsOneSessionsAndRefusesBeforeWriting(t *testing.T) {
	ctx := t.Context()
	sMain := SessionKey{SessionID: "s"}
	sArtifact := SessionKey{SessionID: "s", Subpath: "artifact"}
	foreign := SessionKey{SessionID: "x", Subpath: "artifact"}

	seed := func(t *testing.T) *InMemorySessionStore {
		t.Helper()

		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"before"}`)}},
			{Key: sArtifact, Entries: []SessionStoreEntry{json.RawMessage(`{"artifact":"before"}`)}},
		}))

		return store
	}

	requireUntouched := func(t *testing.T, store *InMemorySessionStore) {
		t.Helper()

		for key, expected := range map[SessionKey]string{
			sMain:     `{"generation":"before"}`,
			sArtifact: `{"artifact":"before"}`,
		} {
			stored, err := store.Load(ctx, key)
			require.NoError(t, err)
			require.Len(t, stored, 1, "a refused replacement wrote to %v", key)
			require.JSONEq(t, expected, string(stored[0]), "a refused replacement rewrote %v", key)
		}

		stored, err := store.Load(ctx, foreign)
		require.NoError(t, err)
		require.Empty(t, stored, "a refused replacement wrote to an unaddressed session")

		subkeys, err := store.ListSubkeys(ctx, sMain)
		require.NoError(t, err)
		require.Equal(t, []string{"artifact"}, subkeys, "a refused replacement changed the addressed subtree")
	}

	t.Run("foreign session id", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"refused"}`)}},
			{Key: foreign, Entries: []SessionStoreEntry{json.RawMessage(`{"x":true}`)}},
		})
		// The refusal names the key it refused and the session that was addressed.
		require.ErrorContains(t, err, `replacement key for a foreign session: session "x" subpath "artifact", addressed session "s"`)
		requireUntouched(t, store)
	})

	t.Run("foreign main key", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: SessionKey{SessionID: "x"}, Entries: []SessionStoreEntry{json.RawMessage(`{"x":true}`)}},
		})
		require.ErrorContains(t, err, `replacement key for a foreign session: session "x" subpath "", addressed session "s"`)
		requireUntouched(t, store)
	})

	t.Run("duplicate key", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"refused"}`)}},
			{Key: sArtifact, Entries: []SessionStoreEntry{json.RawMessage(`{"artifact":"one"}`)}},
			{Key: sArtifact, Entries: []SessionStoreEntry{json.RawMessage(`{"artifact":"two"}`)}},
		})
		require.ErrorContains(t, err, `duplicate replacement key: session "s" subpath "artifact"`)
		requireUntouched(t, store)
	})

	t.Run("duplicate main key", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"one"}`)}},
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"two"}`)}},
		})
		require.ErrorContains(t, err, `duplicate replacement key: session "s" subpath ""`)
		requireUntouched(t, store)
	})

	t.Run("no main key", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sArtifact, Entries: []SessionStoreEntry{json.RawMessage(`{"artifact":"only"}`)}},
		})
		require.ErrorContains(t, err, "replacements must include addressed main key exactly once")
		requireUntouched(t, store)
	})

	t.Run("empty replacement session id", func(t *testing.T) {
		store := seed(t)
		err := store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"refused"}`)}},
			{Key: SessionKey{Subpath: "artifact"}, Entries: []SessionStoreEntry{json.RawMessage(`{}`)}},
		})
		require.ErrorContains(t, err, "replacement session id is required")
		requireUntouched(t, store)
	})

	// An accepted set replaces the addressed session's whole subtree, and a
	// subpath it no longer lists does not survive as a stale sibling.
	t.Run("accepted set replaces the whole subtree", func(t *testing.T) {
		store := seed(t)
		require.NoError(t, store.Replace(ctx, sMain, []SessionStoreReplacement{
			{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"after"}`)}},
			{Key: SessionKey{SessionID: "s", Subpath: "other"}, Entries: []SessionStoreEntry{json.RawMessage(`{"other":true}`)}},
		}))

		stored, err := store.Load(ctx, sMain)
		require.NoError(t, err)
		require.JSONEq(t, `{"generation":"after"}`, string(stored[0]))

		stored, err = store.Load(ctx, sArtifact)
		require.NoError(t, err)
		require.Empty(t, stored, "a subpath the new set omits survived the replacement")

		subkeys, err := store.ListSubkeys(ctx, sMain)
		require.NoError(t, err)
		require.Equal(t, []string{"other"}, subkeys)
	})
}

// TestInMemoryStoreEnforcesTombstoneFinality proves the store itself is where a
// deletion becomes final. Append and Replace addressed to a key Delete
// tombstoned write nothing, clear nothing, and answer success: the deleted state
// is already the caller's answer, and a store that left the rule to the adapter
// above it would resurrect a session whenever a settlement raced the delete that
// had already answered for it.
func TestInMemoryStoreEnforcesTombstoneFinality(t *testing.T) {
	ctx := context.Background()
	main := SessionKey{SessionID: "s1", Subpath: SessionStoreMainSubpath}
	subpath := SessionKey{SessionID: "s1", Subpath: "idmap"}
	bundle := SessionStoreEntry(`{"format":"opencode-sync-events-v1"}`)

	seed := func(t *testing.T) *InMemorySessionStore {
		t.Helper()

		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
			{Key: subpath, Entries: []SessionStoreEntry{bundle}},
		}))
		require.NoError(t, store.Delete(ctx, main))

		return store
	}

	requireStillDeleted := func(t *testing.T, store *InMemorySessionStore) {
		t.Helper()

		entries, err := store.Load(ctx, main)
		require.NoError(t, err)
		require.Empty(t, entries, "a write over a tombstone recreated the main key")

		entries, err = store.Load(ctx, subpath)
		require.NoError(t, err)
		require.Empty(t, entries, "a write over a tombstone recreated a subpath")

		summaries, err := store.ListSessions(ctx)
		require.NoError(t, err)
		require.Empty(t, summaries, "a write over a tombstone made the session listable again")

		subkeys, err := store.ListSubkeys(ctx, main)
		require.NoError(t, err)
		require.Empty(t, subkeys, "a write over a tombstone made a subpath visible again")
	}

	t.Run("append to a tombstoned main key", func(t *testing.T) {
		store := seed(t)
		require.NoError(t, store.Append(ctx, main, []SessionStoreEntry{bundle}))
		requireStillDeleted(t, store)
	})

	t.Run("append to a tombstoned subpath", func(t *testing.T) {
		store := seed(t)
		require.NoError(t, store.Append(ctx, subpath, []SessionStoreEntry{bundle}))
		requireStillDeleted(t, store)
	})

	t.Run("replace over a tombstoned session", func(t *testing.T) {
		store := seed(t)
		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
			{Key: subpath, Entries: []SessionStoreEntry{bundle}},
		}))
		requireStillDeleted(t, store)
	})

	// A sibling session's own deletion is not the addressed session's business:
	// a Replace is one session's, so a delete of another id can neither refuse
	// this call nor be resurrected by it.
	t.Run("a sibling session's tombstone does not reach the addressed one", func(t *testing.T) {
		store := NewInMemorySessionStore()
		sibling := SessionKey{SessionID: "s2", Subpath: SessionStoreMainSubpath}

		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
		}))
		require.NoError(t, store.Replace(ctx, sibling, []SessionStoreReplacement{
			{Key: sibling, Entries: []SessionStoreEntry{bundle}},
		}))
		require.NoError(t, store.Delete(ctx, sibling))

		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
		}))

		entries, err := store.Load(ctx, main)
		require.NoError(t, err)
		require.Len(t, entries, 1, "the addressed session was refused because a sibling was deleted")

		entries, err = store.Load(ctx, sibling)
		require.NoError(t, err)
		require.Empty(t, entries, "a deleted sibling was resurrected by another session's replacement")
	})
}
