package opencodeacp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestRestoreOwnershipRegistryFailureAndSuccessShapes(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	client.xdg.Root = ""
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	node := snapshot.Graph[0]
	require.Error(t, recordSnapshotOwnership(client, snapshot))
	_, err := snapshotRestoreGeneration(client, node)
	require.Error(t, err)
	require.Error(t, claimRestoreOwnership(client, snapshot, nil))
	require.Error(t, verifyRestoreOwnership(client, snapshot, node))

	state := t.TempDir()
	client.xdg.Root = filepath.Join(state, "runtime")
	registry, err := readRestoreOwnership(client)
	require.NoError(t, err)
	require.Empty(t, registry.Aggregates)
	require.NoError(t, recordSnapshotOwnership(client, snapshot))
	require.NoError(t, claimRestoreOwnership(client, snapshot, nil))
	require.NoError(t, verifyRestoreOwnership(client, snapshot, node))
	generation, err := snapshotRestoreGeneration(client, node)
	require.NoError(t, err)
	require.Equal(t, snapshot.RestoreGeneration, generation)

	path := filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
	_, err = readRestoreOwnership(client)
	require.ErrorContains(t, err, "decode restore ownership")
	_, err = snapshotRestoreGeneration(client, node)
	require.ErrorContains(t, err, "decode restore ownership")
	require.NoError(t, os.WriteFile(path, []byte(`{"format":"wrong","aggregates":{}}`), 0o600))
	_, err = readRestoreOwnership(client)
	require.ErrorContains(t, err, "invalid restore ownership")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err = readRestoreOwnership(client)
	require.ErrorContains(t, err, "read restore ownership")

	client.xdg.Root = filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.WriteFile(restoreOwnershipDirectory(client), []byte("x"), 0o600))
	err = writeRestoreOwnership(client, restoreOwnershipFile{})
	require.ErrorContains(t, err, "create restore ownership directory")
}

func preserveRestoreOwnershipSeams(t *testing.T) {
	t.Helper()
	readFile := restoreReadFile
	mkdirAll := restoreMkdirAll
	marshal := restoreJSONMarshal
	createTemp := restoreCreateTemp
	chmod := restoreChmod
	write := restoreWrite
	syncFile := restoreSync
	closeFile := restoreClose
	rename := restoreRename
	open := restoreOpen
	t.Cleanup(func() {
		restoreReadFile = readFile
		restoreMkdirAll = mkdirAll
		restoreJSONMarshal = marshal
		restoreCreateTemp = createTemp
		restoreChmod = chmod
		restoreWrite = write
		restoreSync = syncFile
		restoreClose = closeFile
		restoreRename = rename
		restoreOpen = open
	})
}

func TestRestoreOwnershipRemainingPropagationConflictAndLossBranches(t *testing.T) {
	client := newFakeOpenCodeClient(t)
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	node := snapshot.Graph[0]
	path := filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
	require.Error(t, recordSnapshotOwnership(client, snapshot))
	require.Error(t, claimRestoreOwnership(client, snapshot, nil))
	require.Error(t, verifyRestoreOwnership(client, snapshot, node))

	require.NoError(t, os.WriteFile(path, []byte(`{"format":"opencode-restore-ownership-v1","aggregates":{"native":{"sessionId":"other","restoreGeneration":"other","sourceAggregateId":"native","destinationAggregateId":"native"}}}`), 0o600))
	require.ErrorContains(t, claimRestoreOwnership(client, snapshot, nil), "owned by another restore")
	require.ErrorContains(t, verifyRestoreOwnership(client, snapshot, node), "lost durable restore ownership")
	_, err := snapshotRestoreGeneration(client, node)
	require.ErrorContains(t, err, "owned by another restore")
}

func TestWriteRestoreOwnershipEveryInjectedFilesystemFailure(t *testing.T) {
	registry := restoreOwnershipFile{Format: "opencode-restore-ownership-v1", Aggregates: map[string]restoreOwnership{}}

	t.Run("mkdir", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "mkdir failed")
	})
	t.Run("marshal", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "marshal failed")
	})
	t.Run("create temp", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreCreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("create failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "create failed")
	})
	t.Run("chmod", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreChmod = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "chmod failed")
	})
	t.Run("write", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreWrite = func(*os.File, []byte) (int, error) { return 0, errors.New("write failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "write failed")
	})
	t.Run("sync", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreSync = func(*os.File) error { return errors.New("sync failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "sync failed")
	})
	t.Run("close", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		originalClose := restoreClose
		restoreClose = func(file *os.File) error {
			_ = originalClose(file)

			return errors.New("close failed")
		}
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "close failed")
	})
	t.Run("rename", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreRename = func(string, string) error { return errors.New("rename failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "rename failed")
	})
	t.Run("open directory", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient(t)
		restoreOpen = func(string) (*os.File, error) { return nil, errors.New("open failed") }
		requireDirectoryFlushOutcome(t, writeRestoreOwnership(client, registry), "open failed")
	})
}
func TestActiveReplacementAndArtifactLoadEdges(t *testing.T) {
	t.Run("zero replacement timeout takes the default", func(t *testing.T) {
		cwd := t.TempDir()
		store := NewInMemorySessionStore()
		client := newFakeOpenCodeClient(t)
		agent := NewAgent(WithSessionStore(store))
		agent.sessionReplacementTimeout = 0
		current := testSession(t, agent, client)
		current.cwd = cwd
		current.carrier = newSessionCarrier(map[string]string{"COLOR": "old"}, nil)
		require.NoError(t, current.snapshotToStore(t.Context()))
		client.closeErr = errors.Join(errors.New("still live"), ErrContainmentIncomplete)

		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(current.id, cwd,
			WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{"COLOR": "new"}))),
		))
		require.ErrorIs(t, err, ErrContainmentIncomplete)
	})

	t.Run("replacement detects a changed active mapping", func(t *testing.T) {
		cwd := t.TempDir()
		store := NewInMemorySessionStore()
		client := newFakeOpenCodeClient(t)
		agent := NewAgent(WithSessionStore(store))
		current := testSession(t, agent, client)
		current.cwd = cwd
		current.carrier = newSessionCarrier(map[string]string{"COLOR": "old"}, nil)
		require.NoError(t, current.snapshotToStore(t.Context()))
		replacement := &session{id: current.id}
		client.closeHook = func() {
			agent.mu.Lock()
			agent.sessions[current.id] = replacement
			agent.mu.Unlock()
		}

		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(current.id, cwd,
			WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{"COLOR": "new"}))),
		))
		require.Equal(t, map[string]any{
			jsonFieldError: valInternalFailure,
			jsonFieldClass: classSessionReplacementRaced,
		}, requireInternalErrorData(t, err))
	})

	t.Run("missing image artifact blocks hydration", func(t *testing.T) {
		cwd := t.TempDir()
		snapshot := validSyncSnapshot("session", "native", cwd)
		snapshot.Events["native"] = append(snapshot.Events["native"], opencode.SyncEvent{
			ID: "part", AggregateID: "native", Sequence: 1, Type: syncTypeMessagePartUpdated,
			Data: map[string]json.RawMessage{
				syncFieldSessionID: json.RawMessage(`"native"`),
				syncFieldPart: json.RawMessage(
					`{"url":"` + imageArtifactReferenceScheme + `missing"}`,
				),
				jsonFieldTime: json.RawMessage(`1`),
			},
		})
		entry, err := json.Marshal(snapshot)
		require.NoError(t, err)
		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{entry},
		}}))
		agent := NewAgent(WithSessionStore(store))
		_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest("session", cwd))
		require.ErrorContains(t, err, outputReasonStorageFailed)
	})
}
