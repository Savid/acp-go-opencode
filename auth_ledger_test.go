package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type stubLedgerFile struct {
	name     string
	writeErr error
	chmodErr error
	syncErr  error
	closeErr error
	closed   int
}

func (f *stubLedgerFile) Name() string              { return f.name }
func (f *stubLedgerFile) Write([]byte) (int, error) { return 0, f.writeErr }
func (f *stubLedgerFile) Chmod(os.FileMode) error   { return f.chmodErr }
func (f *stubLedgerFile) Sync() error               { return f.syncErr }
func (f *stubLedgerFile) Close() error {
	f.closed++

	return f.closeErr
}

func newTestLedger(t *testing.T) *authLedger {
	t.Helper()

	ledger, err := newAuthLedger(Options{ProviderAuthRoot: t.TempDir(), Home: t.TempDir()})
	require.NoError(t, err)

	return ledger
}

func TestNewAuthLedgerValidatesTheConfiguredRoot(t *testing.T) {
	root := t.TempDir()

	ledger, err := newAuthLedger(Options{ProviderAuthRoot: root, Home: "/home/opencode"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, authLedgerVendorDir, authLedgerHomeKey("/home/opencode"), authLedgerLeafDir), ledger.dir)

	info, err := os.Stat(ledger.dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(authLedgerDirMode), info.Mode().Perm())
}

func TestNewAuthLedgerRejectsUnusableRoots(t *testing.T) {
	failure := errors.New("refused")

	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "relative root"},
		{name: "mkdir fails", setup: func(t *testing.T) {
			t.Helper()

			ledgerMkdirAll = func(string, fs.FileMode) error { return failure }
			t.Cleanup(func() { ledgerMkdirAll = os.MkdirAll })
		}},
		{name: "chmod fails", setup: func(t *testing.T) {
			t.Helper()

			ledgerChmod = func(string, fs.FileMode) error { return failure }
			t.Cleanup(func() { ledgerChmod = os.Chmod })
		}},
		{name: "stat fails", setup: func(t *testing.T) {
			t.Helper()

			ledgerStat = func(string) (fs.FileInfo, error) { return nil, failure }
			t.Cleanup(func() { ledgerStat = os.Stat })
		}},
		{name: "not a directory", setup: func(t *testing.T) {
			t.Helper()

			t.Helper()

			ledgerStat = func(name string) (fs.FileInfo, error) {
				file := filepath.Join(t.TempDir(), "entry")
				require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

				return os.Stat(file)
			}
			t.Cleanup(func() { ledgerStat = os.Stat })
		}},
		{name: "not writable", setup: func(t *testing.T) {
			t.Helper()

			ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, failure }
			t.Cleanup(func() {
				ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) { return os.CreateTemp(dir, pattern) }
			})
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			if testCase.setup == nil {
				root = "relative/root"
			} else {
				testCase.setup(t)
			}

			_, err := newAuthLedger(Options{ProviderAuthRoot: root, Home: t.TempDir()})
			require.Error(t, err)
		})
	}
}

func TestAuthLedgerRoundTrip(t *testing.T) {
	ledger := newTestLedger(t)

	record, ok, err := ledger.read("xai")
	require.NoError(t, err)
	require.False(t, ok)
	require.Zero(t, record)

	require.NoError(t, ledger.write(authLedgerRecord{ProviderID: "xai", ConnectionID: "c1", State: authLedgerIntent}))

	stored, ok, err := ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "c1", stored.ConnectionID)

	info, err := os.Stat(ledger.path("xai"))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(authLedgerFileMode), info.Mode().Perm())

	require.NoError(t, ledger.write(authLedgerRecord{ProviderID: "deepseek", State: authLedgerConfirmed}))

	records, err := ledger.list()
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "deepseek", records[0].ProviderID)
	require.Equal(t, "xai", records[1].ProviderID)
}

func TestAuthLedgerNeverStoresValues(t *testing.T) {
	ledger := newTestLedger(t)

	require.NoError(t, ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "c1", Revision: 2, BindingGeneration: 3,
		FlowID: "flow", AuthorizeRequestID: "req", State: authLedgerConfirmed,
		CreatedAt: 1, UpdatedAt: 2,
	}))

	contents, err := os.ReadFile(ledger.path("xai"))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, jsonUnmarshalTest(contents, &decoded))
	require.Equal(t, []string{
		"authorizeRequestId", "bindingGeneration", "connectionId", "createdAt",
		"flowId", "providerId", "revision", "state", "updatedAt",
	}, sortedKeys(decoded))
}

func TestAuthLedgerReadFailures(t *testing.T) {
	ledger := newTestLedger(t)

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("io") }
	t.Cleanup(func() { ledgerReadFile = os.ReadFile })

	_, _, err := ledger.read("xai")
	require.Error(t, err)

	ledgerReadFile = func(string) ([]byte, error) { return []byte("{"), nil }

	_, _, err = ledger.read("xai")
	require.Error(t, err)
}

func TestAuthLedgerWriteFailures(t *testing.T) {
	ledger := newTestLedger(t)

	restore := func() {
		ledgerCreateTemp = func(dir string, pattern string) (ledgerFile, error) { return os.CreateTemp(dir, pattern) }
		ledgerRename = os.Rename
		ledgerRemove = os.Remove
		ledgerOpen = os.Open
	}
	t.Cleanup(restore)

	ledgerRemove = func(string) error { return nil }

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return nil, errors.New("create") }
	require.Error(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))

	for _, stub := range []*stubLedgerFile{
		{name: "temp", writeErr: errors.New("write")},
		{name: "temp", chmodErr: errors.New("chmod")},
		{name: "temp", syncErr: errors.New("sync")},
		{name: "temp", closeErr: errors.New("close")},
	} {
		ledgerCreateTemp = func(string, string) (ledgerFile, error) { return stub, nil }
		require.Error(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))
		require.Positive(t, stub.closed)
	}

	ledgerCreateTemp = func(string, string) (ledgerFile, error) { return &stubLedgerFile{name: "temp"}, nil }
	ledgerRename = func(string, string) error { return errors.New("rename") }
	require.Error(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))

	ledgerRename = func(string, string) error { return nil }
	ledgerOpen = func(string) (*os.File, error) { return nil, errors.New("open") }
	require.Error(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))
}

func TestAuthLedgerListFailures(t *testing.T) {
	ledger := newTestLedger(t)

	t.Cleanup(func() {
		ledgerReadDir = os.ReadDir
		ledgerReadFile = os.ReadFile
	})

	ledgerReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("readdir") }

	_, err := ledger.list()
	require.Error(t, err)

	ledgerReadDir = os.ReadDir

	require.NoError(t, os.Mkdir(filepath.Join(ledger.dir, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ledger.dir, "other.txt"), []byte("x"), 0o600))

	records, err := ledger.list()
	require.NoError(t, err)
	require.Empty(t, records)

	require.NoError(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("io") }

	_, err = ledger.list()
	require.Error(t, err)

	ledgerReadFile = func(string) ([]byte, error) { return []byte("{"), nil }

	_, err = ledger.list()
	require.Error(t, err)
}

func TestAuthProofSourceIsTotalOverLedgerAndProbe(t *testing.T) {
	cases := []struct {
		state   string
		present bool
		want    string
	}{
		{state: authLedgerConfirmed, present: true, want: authProofConfirmedPresent},
		{state: authLedgerConfirmed, present: false, want: authProofConfirmedAbsent},
		{state: authLedgerIntent, present: true, want: authProofNotConfirmed},
		{state: authLedgerIntent, present: false, want: authProofNotConfirmed},
		{state: "", present: true, want: authProofNotConfirmed},
		{state: "", present: false, want: authProofNotConfirmed},
	}

	for _, testCase := range cases {
		require.Equal(t, testCase.want, authProofSource(testCase.state, testCase.present))
	}
}

func TestInventoryReadsTheLedgerAndProbes(t *testing.T) {
	harness := newAuthAgent(t)
	broker, client, session := harness.broker, harness.runtime, harness.session

	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "xai", ConnectionID: "c1", Revision: 2, BindingGeneration: 3, State: authLedgerConfirmed}))
	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "deepseek", ConnectionID: "c2", State: authLedgerIntent}))
	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "gone", ConnectionID: "c3", State: authLedgerRemoved}))

	client.storedAuth = map[string]opencode.ProviderAuthCredential{
		"xai":      {Type: opencode.ProviderAuthTypeOAuth},
		"deepseek": {Type: opencode.ProviderAuthTypeAPI},
	}

	params := mustJSON(t, map[string]any{authFieldSessionID: string(session.id)})

	result, err := broker.inventory(context.Background(), params)
	require.NoError(t, err)
	require.Equal(t, authInventoryResult{Entries: []authInventoryEntry{
		{ProviderID: "deepseek", ConnectionID: "c2", ProofSource: authProofNotConfirmed},
		{ProviderID: "xai", ConnectionID: "c1", Revision: 2, BindingGeneration: 3, ProofSource: authProofConfirmedPresent},
	}}, result)
}

func TestInventoryFailures(t *testing.T) {
	harness := newAuthAgent(t)
	broker, client, session := harness.broker, harness.runtime, harness.session

	_, err := broker.inventory(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")

	_, err = broker.inventory(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: ""}))
	requireInvalidParams(t, err, authFieldSessionID)

	_, err = broker.inventory(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: "missing"}))
	requireInvalidParams(t, err, jsonFieldSessionID)

	params := mustJSON(t, map[string]any{authFieldSessionID: string(session.id)})

	ledgerReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("readdir") }

	_, err = broker.inventory(context.Background(), params)
	requireAuthFailure(t, err, authCauseHarvestFailed)

	ledgerReadDir = os.ReadDir

	require.NoError(t, broker.ledger.write(authLedgerRecord{ProviderID: "xai", State: authLedgerConfirmed}))

	client.storedAuthErr = errors.New("io")

	_, err = broker.inventory(context.Background(), params)
	requireAuthFailure(t, err, authCauseHarvestFailed)

	client.storedAuthErr = nil

	session.mu.Lock()
	session.client = nil
	session.mu.Unlock()

	_, err = broker.inventory(context.Background(), params)
	requireAuthFailure(t, err, authCauseTransport)
}

func jsonUnmarshalTest(data []byte, out any) error {
	return json.Unmarshal(data, out)
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

func TestAuthLedgerWriteReportsAnEncodingFailure(t *testing.T) {
	ledger := newTestLedger(t)

	original := ledgerMarshal
	ledgerMarshal = func(any) ([]byte, error) { return nil, errors.New("encode") }

	t.Cleanup(func() { ledgerMarshal = original })

	require.Error(t, ledger.write(authLedgerRecord{ProviderID: "xai"}))
}
