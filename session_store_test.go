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

func TestInMemoryStoreReplaceRequiresOneMainPerNamedMemberAtomically(t *testing.T) {
	ctx := t.Context()
	store := NewInMemorySessionStore()
	sMain := SessionKey{SessionID: "s"}
	xMain := SessionKey{SessionID: "x"}
	require.NoError(t, store.Replace(ctx, sMain, []SessionStoreReplacement{{
		Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"before"}`)},
	}}))

	err := store.Replace(ctx, sMain, []SessionStoreReplacement{
		{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"refused"}`)}},
		{Key: SessionKey{SessionID: "x", Subpath: "artifact"}, Entries: []SessionStoreEntry{json.RawMessage(`{"x":true}`)}},
	})
	require.ErrorContains(t, err, `exactly one main key for session "x"`)

	entries, loadErr := store.Load(ctx, sMain)
	require.NoError(t, loadErr)
	require.JSONEq(t, `{"generation":"before"}`, string(entries[0]))
	xArtifact, loadErr := store.Load(ctx, SessionKey{SessionID: "x", Subpath: "artifact"})
	require.NoError(t, loadErr)
	require.Empty(t, xArtifact, "refused replacement mutated an unaddressed member")

	require.NoError(t, store.Replace(ctx, sMain, []SessionStoreReplacement{
		{Key: sMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"after"}`)}},
		{Key: SessionKey{SessionID: "s", Subpath: "artifact"}, Entries: []SessionStoreEntry{json.RawMessage(`{"s":true}`)}},
		{Key: xMain, Entries: []SessionStoreEntry{json.RawMessage(`{"generation":"x"}`)}},
		{Key: SessionKey{SessionID: "x", Subpath: "artifact"}, Entries: []SessionStoreEntry{json.RawMessage(`{"x":true}`)}},
	}))

	for key, expected := range map[SessionKey]string{
		sMain:                                 `{"generation":"after"}`,
		{SessionID: "s", Subpath: "artifact"}: `{"s":true}`,
		xMain:                                 `{"generation":"x"}`,
		{SessionID: "x", Subpath: "artifact"}: `{"x":true}`,
	} {
		stored, storedErr := store.Load(ctx, key)
		require.NoError(t, storedErr)
		require.Len(t, stored, 1)
		require.JSONEq(t, expected, string(stored[0]))
	}
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

	// A graph replacement carries one member per session. A member the host
	// deleted on its own drops out of the set; the addressed session is still
	// written, because the delete answered for that one id and no other.
	t.Run("replace drops a separately deleted graph member", func(t *testing.T) {
		store := NewInMemorySessionStore()
		child := SessionKey{SessionID: "s2", Subpath: SessionStoreMainSubpath}

		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
			{Key: child, Entries: []SessionStoreEntry{bundle}},
		}))
		require.NoError(t, store.Delete(ctx, child))

		require.NoError(t, store.Replace(ctx, main, []SessionStoreReplacement{
			{Key: main, Entries: []SessionStoreEntry{bundle}},
			{Key: child, Entries: []SessionStoreEntry{bundle}},
		}))

		entries, err := store.Load(ctx, main)
		require.NoError(t, err)
		require.Len(t, entries, 1, "the addressed session was refused along with the deleted member")

		entries, err = store.Load(ctx, child)
		require.NoError(t, err)
		require.Empty(t, entries, "a deleted graph member was resurrected by its parent's replacement")
	})
}
