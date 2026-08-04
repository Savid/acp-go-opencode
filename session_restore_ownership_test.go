package opencodeacp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestoreOwnershipRegistryFailureAndSuccessShapes(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.xdg.Root = ""
	snapshot := validSyncSnapshot("session", "native", "/source")
	node := snapshot.Graph[0]
	require.Error(t, recordSnapshotOwnership(client, snapshot))
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

	path := filepath.Join(restoreOwnershipDirectory(client), restoreOwnershipFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{`), 0o600))
	_, err = readRestoreOwnership(client)
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
	client := newFakeOpenCodeClient()
	snapshot := validSyncSnapshot("session", "native", "/source")
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
}

func TestWriteRestoreOwnershipEveryInjectedFilesystemFailure(t *testing.T) {
	registry := restoreOwnershipFile{Format: "opencode-restore-ownership-v1", Aggregates: map[string]restoreOwnership{}}

	t.Run("mkdir", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "mkdir failed")
	})
	t.Run("marshal", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "marshal failed")
	})
	t.Run("create temp", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreCreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("create failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "create failed")
	})
	t.Run("chmod", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreChmod = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "chmod failed")
	})
	t.Run("write", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreWrite = func(*os.File, []byte) (int, error) { return 0, errors.New("write failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "write failed")
	})
	t.Run("sync", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreSync = func(*os.File) error { return errors.New("sync failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "sync failed")
	})
	t.Run("close", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		originalClose := restoreClose
		restoreClose = func(file *os.File) error {
			_ = originalClose(file)

			return errors.New("close failed")
		}
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "close failed")
	})
	t.Run("rename", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreRename = func(string, string) error { return errors.New("rename failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "rename failed")
	})
	t.Run("open directory", func(t *testing.T) {
		preserveRestoreOwnershipSeams(t)
		client := newFakeOpenCodeClient()
		restoreOpen = func(string) (*os.File, error) { return nil, errors.New("open failed") }
		require.ErrorContains(t, writeRestoreOwnership(client, registry), "open failed")
	})
}
