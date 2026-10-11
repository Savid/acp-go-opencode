package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// decodeRows decodes mirror rows into their events.
func decodeRows(t *testing.T, rows [][]byte) []opencode.SyncEvent {
	t.Helper()

	events := make([]opencode.SyncEvent, 0, len(rows))
	for _, row := range rows {
		var event opencode.SyncEvent
		require.NoError(t, json.Unmarshal(row, &event))
		events = append(events, event)
	}

	return events
}

// replaceStored publishes rows and record as the session's whole generation.
func replaceStored(t *testing.T, store acpcore.SessionStore, id string, rows [][]byte, record sessionRecord) {
	t.Helper()

	require.NoError(t, sessionlog.NewMirror(store, id).Commit(t.Context(), sessionlog.Rows(rows), func(sessionlog.Tail) (any, error) { return record, nil }))
}

// Native writes that land while a commit reads, as opencode keeps writing
// after a turn reports idle, join that commit or the next one; they never
// fail the turn.
func TestMirrorKeepsNativeWritesAfterIdle(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"once", "child", "always"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "history-advance")
			store := acpcore.NewInMemorySessionStore()
			a := NewAgent(testOptions(t,
				WithEnv(map[string]string{fakeOpenCodeEnv: "1", fakeOpenCodeEnvHistoryAdvance: marker, "GORACE": os.Getenv("GORACE") + " atexit_sleep_ms=0"}),
				WithSessionStore(store),
			)...)
			t.Cleanup(func() { _ = a.Close() })
			_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
			require.NoError(t, err)
			created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
			require.NoError(t, err)
			s, err := a.session(t.Context(), created.SessionId)
			require.NoError(t, err)
			var record sessionRecord
			before, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
			require.NoError(t, err)
			require.True(t, found)
			require.NoError(t, os.WriteFile(marker, []byte(mode), 0o600))

			for range 2 {
				_, err = a.Prompt(t.Context(), wire.TextPromptRequest(created.SessionId, "HELLO"))
				require.NoError(t, err)
			}

			after, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, before, after[:len(before)], "a commit extends the stored history")
			_, err = checkSync(after, s.nativeID, nil)
			require.NoError(t, err)

			wantTitle, wantLate := "late history row", 1
			switch mode {
			case "child":
				wantTitle = "late history child"
			case "always":
				wantLate = 2
			}

			late := 0
			for _, event := range decodeRows(t, after) {
				info := fakeSessionInfo(event)
				if info.Title != wantTitle {
					continue
				}
				if mode == "child" {
					require.Equal(t, s.nativeID, info.ParentID)
				} else {
					require.Equal(t, s.nativeID, event.AggregateID)
				}
				late++
			}
			require.Equal(t, wantLate, late)
		})
	}
}

func TestMirrorRefusesWhileChildSessionRuns(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	a := NewAgent(testOptions(t, WithSessionStore(store))...)
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.binding
	s.mu.Unlock()
	var child opencode.NativeSession
	require.NoError(t, rt.client.Do(t.Context(), cwd, http.MethodPost, "/session", map[string]string{"parentID": s.nativeID}, &child))

	promptCtx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		request := opencode.MessageRequest{MessageID: opencode.NewMessageID(), Parts: []map[string]any{{"type": "text", "text": "SLOW"}}}
		done <- rt.client.Do(promptCtx, cwd, http.MethodPost, opencode.SessionPath(child.ID)+"/message", request, nil)
	}()
	require.Eventually(t, func() bool {
		var statuses map[string]opencode.NativeSessionStatus
		if rt.client.Do(t.Context(), cwd, http.MethodGet, "/session/status", nil, &statuses) != nil {
			return false
		}

		return statuses[child.ID].Type == "busy"
	}, 5*time.Second, 10*time.Millisecond)

	require.ErrorContains(t, s.commitMirror(t.Context(), rt), "native session is still running")

	stop()
	<-done
	require.NoError(t, s.commitMirror(t.Context(), rt))
	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	_, err = checkSync(rows, s.nativeID, nil)
	require.NoError(t, err)
	childRows := 0
	for _, event := range decodeRows(t, rows) {
		if event.AggregateID == child.ID {
			childRows++
		}
	}
	require.Greater(t, childRows, 1)
}

func TestMirrorIncludesDescendantsAndExcludesUnrelatedSessions(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	a := NewAgent(testOptions(t, WithSessionStore(store))...)
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	root, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	s, err := a.session(t.Context(), root.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.binding
	s.mu.Unlock()
	parent := s.nativeID
	want := make([]string, 1, 3)
	want[0] = parent
	for range 2 {
		var child opencode.NativeSession
		require.NoError(t, rt.client.Do(t.Context(), cwd, http.MethodPost, "/session", map[string]string{"parentID": parent}, &child))
		want = append(want, child.ID)
		parent = child.ID
	}
	unrelated, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	require.NoError(t, s.commitMirror(t.Context(), rt))
	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(root.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	synced, err := checkSync(rows, s.nativeID, nil)
	require.NoError(t, err)
	graph := slices.Collect(maps.Keys(synced))
	require.Len(t, graph, len(want))
	for _, id := range want {
		require.Contains(t, graph, id)
	}
	require.NotContains(t, graph, string(unrelated.SessionId))
}

// A commit the session cannot attempt, and one with no complete native
// snapshot to replace, both fail rather than report a success the store does
// not hold.
func TestMirrorCommitRefusesWhatItCannotAttempt(t *testing.T) {
	t.Parallel()

	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)

	require.Error(t, s.commitMirror(t.Context(), nil),
		"a commit with no binding to read the native history through cannot be attempted")

	_, err = a.Prompt(t.Context(), wire.TextPromptRequest(created.SessionId, "FORGET"))
	data := requestErrorData(t, err)
	require.Equal(t, vendor+"_"+wire.TokenTurnFailed, data[wire.FieldError],
		"a turn whose native history vanished is not durable")
	require.Equal(t, wire.CauseTransport, data[wire.FieldCause])
	require.EqualValues(t, 404, data["statusCode"])
	require.Equal(t, "opencode HTTP status 404", data[wire.FieldMessage])
}

// A generation whose native rows do not decode, and one whose captured image
// the record no longer holds, both refuse the load instead of restoring a
// partial session.
func TestRestoreRefusesADamagedMirror(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()

	cwd := t.TempDir()

	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = h.prompt(session.SessionId, "IMAGE", nil)
	require.NoError(t, err)

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	record := storedRecord(t, store, session.SessionId)
	require.NotEmpty(t, record.Artifacts, "the native image is captured beside the rows")

	stored := make([][]byte, 0, len(rows)+1)
	for _, row := range rows {
		stored = append(stored, row)
	}

	gap := fmt.Appendf(nil, `{"id":"evt_gap","aggregate_id":%q,"seq":9999,"type":"message.updated.1","data":{"sessionID":%q}}`,
		session.SessionId, session.SessionId)

	replaceStored(t, store, string(session.SessionId), append(stored, gap), record)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, vendor+"_"+wire.TokenRestoreFailed, requestErrorData(t, err)[wire.FieldError],
		"a native row out of sequence refuses the restore")

	uncaptured := record
	uncaptured.Artifacts = nil

	replaceStored(t, store, string(session.SessionId), stored, uncaptured)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Equal(t, vendor+"_"+wire.TokenRestoreFailed, requestErrorData(t, err)[wire.FieldError],
		"a local image the record no longer captures refuses the restore")
}

// Residual native state with no store entry is neither listed nor adopted.
func TestResidualNativeStateIsNeverAdopted(t *testing.T) {
	t.Parallel()

	home, cwd := filepath.Join(t.TempDir(), "home"), t.TempDir()
	orphan := "ses_orphan0000000000000000"
	native := filepath.Join(home, "data", "opencode", "fake.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o700))
	require.NoError(t, os.WriteFile(native, []byte(`[{"id":"evt_orphan","aggregate_id":"`+orphan+`","seq":0,"type":"session.created.1","data":{"info":{"id":"`+orphan+`","directory":"`+cwd+`","agent":"build","model":{"id":"vision","providerID":"fake"},"time":{"created":1,"updated":1}},"sessionID":"`+orphan+`"}}]`), 0o600))

	h := newHarness(t, WithHome(home))
	h.initialize()

	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)[wire.FieldError])

	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(acp.SessionId(orphan), cwd))
	require.Equal(t, "unknown session", requestErrorData(t, err)[wire.FieldError])
}

type recoveryFaultStore struct {
	acpcore.SessionStore
	fail atomic.Bool
}

func (s *recoveryFaultStore) Replace(ctx context.Context, main acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("injected store failure")
	}

	return s.SessionStore.Replace(ctx, main, replacements)
}

func (s *recoveryFaultStore) Append(ctx context.Context, main acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	if s.fail.Load() {
		return errors.New("injected store failure")
	}

	return s.SessionStore.Append(ctx, main, base, entries, replacements)
}

func TestNativeBindingSurvivesLoadAndResume(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(created.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	var record sessionRecord
	rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, record.NativeSessionID)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), created.Meta)
	id := acp.SessionId("acp-conversation-independent-of-native-id")
	record.SessionID = string(id)
	replaceStored(t, store, string(id), rows, record)
	require.NoError(t, store.Delete(t.Context(), acpcore.SessionKey{SessionID: string(created.SessionId)}))

	before := len(h.rec.snapshot())
	loaded, err := h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, wire.NativeSessionMeta(vendor, record.NativeSessionID), loaded.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	for _, update := range h.rec.snapshot()[before:] {
		require.Equal(t, id, update.SessionId)
	}
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, id, listed.Sessions[0].SessionId)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: id})
	require.NoError(t, err)
	listed, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, loaded.Meta, listed.Sessions[0].Meta)
	resumed, err := h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(id, cwd))
	require.NoError(t, err)
	require.Equal(t, loaded.Meta, resumed.Meta)
	_, err = h.prompt(id, "HELLO", nil)
	require.NoError(t, err)
	var after sessionRecord
	_, found, err = sessionlog.Load(t.Context(), store, string(id), &after)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record.NativeSessionID, after.NativeSessionID)
	require.Equal(t, string(id), after.SessionID)
}

func TestFailedConfigChangeDoesNotReachTheNextCommit(t *testing.T) {
	t.Parallel()
	store := &recoveryFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	session := h.newSession()
	var before sessionRecord
	_, found, err := sessionlog.Load(t.Context(), store, string(session.SessionId), &before)
	require.NoError(t, err)
	require.True(t, found)
	store.fail.Store(true)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configModel, "fake/text"))
	require.Error(t, err)
	store.fail.Store(false)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(session.SessionId, configEffort, "high"))
	require.NoError(t, err)
	var after sessionRecord
	_, found, err = sessionlog.Load(t.Context(), store, string(session.SessionId), &after)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before.Model, after.Model)
}

type firstMirrorFailureStore struct {
	acpcore.SessionStore
	calls atomic.Int32
}

func (s *firstMirrorFailureStore) Replace(ctx context.Context, key acpcore.SessionKey, rows []acpcore.SessionStoreReplacement) error {
	if s.calls.Add(1) == 1 {
		return errors.New("initial mirror unavailable")
	}

	return s.SessionStore.Replace(ctx, key, rows)
}

func TestFailedNewSessionDoesNotPersistDuringCleanup(t *testing.T) {
	t.Parallel()
	store := &firstMirrorFailureStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	response, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, response.SessionId)
	rows, err := store.ListSessions(h.ctx())
	require.NoError(t, err)
	require.Empty(t, rows)
	listed, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
}

type firstOpenFailureClient struct {
	*recorder
	failed atomic.Bool
}

func (c *firstOpenFailureClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if c.failed.CompareAndSwap(false, true) {
		return errors.New("initial publication unavailable")
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestFailedSessionOpenReleasesActiveSlot(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(&firstOpenFailureClient{recorder: newRecorder()}, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	first, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.Error(t, err)
	require.Empty(t, first.SessionId)
	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
}

type blockedLoadStore struct {
	acpcore.SessionStore
	block            atomic.Bool
	entered, release chan struct{}
}

func (s *blockedLoadStore) Load(ctx context.Context, sessionID string) (map[string][]acpcore.SessionStoreEntry, error) {
	rows, err := s.SessionStore.Load(ctx, sessionID)
	if err == nil && s.block.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return rows, err
}

func TestDeleteWinsAgainstPreparedLoad(t *testing.T) {
	t.Parallel()
	store := &blockedLoadStore{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "HELLO", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	store.block.Store(true)
	done := make(chan error, 1)
	ctx := h.ctx()
	go func() {
		_, loadErr := h.conn.LoadSession(ctx, wire.LoadSessionRequest(session.SessionId, cwd))
		done <- loadErr
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("load did not reach stored configuration")
	}
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	close(store.release)
	require.NoError(t, err)
	require.Equal(t, "unknown session", requestErrorData(t, <-done)["error"])
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
}

// countingStore counts the writes a session makes to the store.
type countingStore struct {
	acpcore.SessionStore
	replaces atomic.Int32
	appends  atomic.Int32
	deletes  atomic.Int32
}

func (s *countingStore) Replace(ctx context.Context, main acpcore.SessionKey, replacements []acpcore.SessionStoreReplacement) error {
	s.replaces.Add(1)

	return s.SessionStore.Replace(ctx, main, replacements)
}

func (s *countingStore) Append(ctx context.Context, main acpcore.SessionKey, base int, entries []acpcore.SessionStoreEntry, replacements []acpcore.SessionStoreReplacement) error {
	s.appends.Add(1)

	return s.SessionStore.Append(ctx, main, base, entries, replacements)
}

func (s *countingStore) Delete(ctx context.Context, key acpcore.SessionKey) error {
	s.deletes.Add(1)

	return s.SessionStore.Delete(ctx, key)
}

func ephemeralMeta() wire.SessionRequestOption {
	return wire.WithSessionMeta(wire.SessionMeta{Ephemeral: true}.Apply(nil))
}

func TestEphemeralSessionNeverReachesTheStore(t *testing.T) {
	t.Parallel()

	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()

	ephemeral, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, ephemeralMeta()))
	require.NoError(t, err)
	_, err = h.prompt(ephemeral.SessionId, "probe", nil)
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions, "a live ephemeral session is never listed")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: ephemeral.SessionId})
	require.NoError(t, err)
	_, err = h.conn.UnstableDeleteSession(h.ctx(), wire.DeleteSessionRequest(ephemeral.SessionId))
	require.NoError(t, err)

	require.Zero(t, store.replaces.Load()+store.appends.Load(), "an ephemeral session is never mirrored")
	require.Zero(t, store.deletes.Load(), "an ephemeral session leaves no tombstone")
	list, err = h.conn.ListSessions(h.ctx(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)

	durable, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(durable.SessionId, "kept", nil)
	require.NoError(t, err)
	require.Positive(t, store.replaces.Load(), "a session the host did not mark ephemeral still mirrors")
}

func TestEphemeralSessionMetaIsRefusedWhereItHasNoMeaning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	cwd := t.TempDir()
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd, ephemeralMeta()))
	require.Equal(t, "_meta."+wire.SessionMetaKey, requestErrorData(t, err)[wire.FieldField])
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd, ephemeralMeta()))
	require.Equal(t, "_meta."+wire.SessionMetaKey, requestErrorData(t, err)[wire.FieldField])

	unknown := wire.WithSessionMeta(map[string]any{wire.SessionMetaKey: map[string]any{"persist": false}})
	_, err = h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, unknown))
	require.Equal(t, "_meta."+wire.SessionMetaKey+".persist", requestErrorData(t, err)[wire.FieldField])
}

func TestMirrorFailureFencesTurnAndAllowsRetry(t *testing.T) {
	t.Parallel()

	for _, prompt := range []string{"HELLO", "ERROR"} {
		t.Run(prompt, func(t *testing.T) {
			t.Parallel()

			store := &recoveryFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
			h := newHarness(t, WithSessionStore(store))
			h.initialize(withLifecycle())
			created := h.newSession()
			before, err := store.Load(t.Context(), string(created.SessionId))
			require.NoError(t, err)
			store.fail.Store(true)
			t.Cleanup(func() { store.fail.Store(false) })
			response, err := h.prompt(created.SessionId, prompt, promptMeta(1))
			require.Empty(t, response.StopReason)
			require.Equal(t, -32603, requestErrorCode(t, err))
			data := requestErrorData(t, err)
			require.Equal(t, "opencode_turn_failed", data["error"])
			if prompt == "ERROR" {
				require.Equal(t, "provider", data["cause"])
				require.Equal(t, "provider refused", data["message"])
			} else {
				require.Equal(t, "transport", data["cause"])
				require.Equal(t, "session mirror commit failed", data["message"])
			}
			after, err := store.Load(t.Context(), string(created.SessionId))
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running"}, eventTypes(lifecycleEvents(h.rec.snapshot())))

			store.fail.Store(false)
			response, err = h.prompt(created.SessionId, "HELLO", promptMeta(2))
			require.NoError(t, err)
			require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
			require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, eventTypes(lifecycleEvents(h.rec.snapshot())))
			var streams []string
			for _, update := range h.rec.snapshot() {
				envelope, ok := update.Meta[wire.LifecycleKey].(map[string]any)
				if !ok {
					continue
				}
				event, ok := envelope["event"].(map[string]any)
				if ok && event["type"] == "lifecycle_snapshot" {
					id, ok := envelope["streamId"].(string)
					require.True(t, ok)
					streams = append(streams, id)
				}
			}
			require.Len(t, streams, 2)
			require.NotEqual(t, streams[0], streams[1])
		})
	}
}

// nativeReads counts the native database reads the fake has recorded in path.
func nativeReads(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}

	require.NoError(t, err)

	return strings.Count(string(data), "db\n")
}

// A settled turn reads native history once and appends only the events it
// added; a configuration change reads none and leaves the history as it is;
// a turn reads its own messages, never the whole session.
func TestCommitsReadOnlyNewNativeHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work, messageReads := filepath.Join(dir, "work"), filepath.Join(dir, "message-reads")
	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store), WithEnv(map[string]string{
		fakeOpenCodeEnv: "1", fakeOpenCodeEnvNativeWork: work, fakeOpenCodeEnvMessageReads: messageReads,
		"GORACE": os.Getenv("GORACE") + " atexit_sleep_ms=0",
	}))
	h.initialize()
	created := h.newSession()
	require.Equal(t, 1, nativeReads(t, work), "establishment reads the new graph once")
	require.EqualValues(t, 1, store.replaces.Load())

	stored := func() [][]byte {
		var record sessionRecord
		rows, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
		require.NoError(t, err)
		require.True(t, found)

		return rows
	}

	history := stored()
	for turn := 1; turn <= 3; turn++ {
		_, err := h.prompt(created.SessionId, "MULTI", nil)
		require.NoError(t, err)
		require.Equal(t, 1+turn, nativeReads(t, work), "each settled turn reads native history once")
		require.EqualValues(t, turn, store.appends.Load(), "each settled turn appends")
		require.EqualValues(t, 1, store.replaces.Load(), "no settled turn republishes the history")

		after := stored()
		require.Greater(t, len(after), len(history))
		require.Equal(t, history, after[:len(history)], "a commit extends the stored history")
		history = after
	}

	_, err := h.conn.SetSessionConfigOption(h.ctx(), wire.SetConfigOptionRequest(created.SessionId, configEffort, "high"))
	require.NoError(t, err)
	require.Equal(t, 4, nativeReads(t, work), "a configuration change reads no native history")
	require.EqualValues(t, 4, store.appends.Load())
	require.Equal(t, history, stored(), "a configuration change leaves the history as it is")
	require.Equal(t, "high", storedRecord(t, store, created.SessionId).Effort)

	_, err = os.Stat(messageReads)
	require.ErrorIs(t, err, os.ErrNotExist, "no turn reads every message of its session")

	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Equal(t, 5, nativeReads(t, work), "close reads the events since the last commit")
	require.EqualValues(t, 4, store.appends.Load(), "a commit with no new event and the same configuration writes nothing")
	require.EqualValues(t, 1, store.replaces.Load())
}

// A restore into a home without the session imports each conversation of the
// graph with its own native replay, the parent before its child, and commits
// only the configuration over the adopted history.
func TestRestoreImportsEachConversationParentFirst(t *testing.T) {
	t.Parallel()

	store := &countingStore{SessionStore: acpcore.NewInMemorySessionStore()}
	first := NewAgent(testOptions(t, WithSessionStore(store))...)
	t.Cleanup(func() { _ = first.Close() })
	_, err := first.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	created, err := first.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	s, err := first.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.binding
	s.mu.Unlock()
	var child opencode.NativeSession
	require.NoError(t, rt.client.Do(t.Context(), cwd, http.MethodPost, "/session", map[string]string{"parentID": s.nativeID}, &child))
	_, err = first.Prompt(t.Context(), wire.TextPromptRequest(created.SessionId, "HELLO"))
	require.NoError(t, err)
	require.NoError(t, first.Close())

	var record sessionRecord
	stored, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	synced, err := checkSync(stored, s.nativeID, nil)
	require.NoError(t, err)
	require.Contains(t, synced, child.ID, "the stored graph holds the child")

	work := filepath.Join(t.TempDir(), "work")
	second := NewAgent(testOptions(t, WithSessionStore(store), WithEnv(map[string]string{
		fakeOpenCodeEnv: "1", fakeOpenCodeEnvNativeWork: work, "GORACE": os.Getenv("GORACE") + " atexit_sleep_ms=0",
	}))...)
	t.Cleanup(func() { _ = second.Close() })
	_, err = second.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	replaces := store.replaces.Load()
	_, err = second.LoadSession(t.Context(), wire.LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)

	data, err := os.ReadFile(work)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(data), "import\n"), "one replay per conversation")
	require.Equal(t, replaces, store.replaces.Load(), "a restore never republishes the adopted history")

	restored, _, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.Equal(t, stored, restored[:len(stored)], "the stored history stays a prefix")
}

// A native run that starts between a commit's read and its idle check delays
// the commit; the finished turn still succeeds.
func TestCommitRereadsAfterANativeRunEnds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	work, script := filepath.Join(dir, "work"), filepath.Join(dir, "status")
	h := newHarness(t, WithEnv(map[string]string{
		fakeOpenCodeEnv: "1", fakeOpenCodeEnvNativeWork: work, fakeOpenCodeEnvStatusScript: script,
		"GORACE": os.Getenv("GORACE") + " atexit_sleep_ms=0",
	}))
	h.initialize()
	created := h.newSession()
	reads := nativeReads(t, work)
	require.NoError(t, os.WriteFile(script, []byte("idle busy"), 0o600))

	_, err := h.prompt(created.SessionId, "HELLO", nil)
	require.NoError(t, err)
	require.Equal(t, reads+2, nativeReads(t, work), "the read the run overlapped is taken again")
}

// History reads page through a conversation larger than one page, and a
// restore pages through the native history it compares.
func TestHistoryReadsInPages(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	work := filepath.Join(t.TempDir(), "work")
	env := WithEnv(map[string]string{fakeOpenCodeEnv: "1", fakeOpenCodeEnvNativeWork: work, "GORACE": os.Getenv("GORACE") + " atexit_sleep_ms=0"})
	a := NewAgent(testOptions(t, WithSessionStore(store), env)...)
	a.historyPageBytes = 1
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = a.Prompt(t.Context(), wire.TextPromptRequest(created.SessionId, "MULTI"))
	require.NoError(t, err)

	var record sessionRecord
	stored, found, err := sessionlog.Load(t.Context(), store, string(created.SessionId), &record)
	require.NoError(t, err)
	require.True(t, found)
	_, err = checkSync(stored, string(created.SessionId), nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, nativeReads(t, work), len(stored), "each page holds one event")

	_, err = a.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.LoadSession(t.Context(), wire.LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
}

// A configuration of a session's runtime makes the environment plugin read
// carriers again.
func TestConfigurationAdvancesPluginGeneration(t *testing.T) {
	t.Parallel()

	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	generation := func() string {
		a.runtimeMu.Lock()
		root := a.runtime.root
		a.runtimeMu.Unlock()
		data, readErr := os.ReadFile(filepath.Join(root, "generation"))
		require.NoError(t, readErr)

		return string(data)
	}

	before := generation()
	require.NotEmpty(t, before)
	option := wire.WithSessionMetaValue(map[string]any{"opencode": map[string]any{"options": map[string]any{"env": map[string]string{"CARRIER": "changed"}}}})
	_, err = a.LoadSession(t.Context(), wire.LoadSessionRequest(created.SessionId, cwd, option))
	require.NoError(t, err)
	require.NotEqual(t, before, generation())
}

func TestCheckSync(t *testing.T) {
	t.Parallel()

	row := func(id, aggregate string, seq int64, typ, data string) []byte {
		return fmt.Appendf(nil, `{"id":%q,"aggregate_id":%q,"seq":%d,"type":%q,"data":%s}`, id, aggregate, seq, typ, data)
	}
	created := func(id, aggregate, parent string) []byte {
		return row(id, aggregate, 0, "session.created.1", fmt.Sprintf(`{"sessionID":%q,"info":{"id":%q,"parentID":%q}}`, aggregate, aggregate, parent))
	}
	updated := func(id, aggregate string, seq int64) []byte {
		return row(id, aggregate, seq, "session.updated.1", fmt.Sprintf(`{"sessionID":%q}`, aggregate))
	}

	for _, tc := range []struct {
		name string
		rows [][]byte
		from map[string]int64
		want map[string]int64
		err  string
	}{
		{name: "whole graph", rows: [][]byte{created("e0", "root", ""), created("e1", "child", "root"), updated("e2", "root", 1)}, want: map[string]int64{"root": 1, "child": 0}},
		{name: "continuation", rows: [][]byte{updated("e3", "root", 2), created("e4", "late", "child")}, from: map[string]int64{"root": 1, "child": 0}, want: map[string]int64{"root": 2, "child": 0, "late": 0}},
		{name: "nothing new", from: map[string]int64{"root": 1}, want: map[string]int64{"root": 1}},
		{name: "gap", rows: [][]byte{updated("e3", "root", 3)}, from: map[string]int64{"root": 1}, err: "native sync sequence gap"},
		{name: "replayed sequence", rows: [][]byte{updated("e3", "root", 1)}, from: map[string]int64{"root": 1}, err: "native sync sequence gap"},
		{name: "root missing", rows: [][]byte{created("e1", "child", "root")}, err: "native root session missing"},
		{name: "unrelated", rows: [][]byte{created("e0", "other", "")}, from: map[string]int64{"root": 0}, err: "unrelated native sync aggregate"},
		{name: "identity", rows: [][]byte{row("e1", "root", 1, "session.updated.1", `{"sessionID":"other"}`)}, from: map[string]int64{"root": 0}, err: "native sync identity mismatch"},
		{name: "duplicate", rows: [][]byte{created("e0", "root", ""), updated("e0", "root", 1)}, err: "invalid native sync event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synced, err := checkSync(tc.rows, "root", tc.from)
			if tc.err != "" {
				require.EqualError(t, err, tc.err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, synced)
		})
	}
}

// syntheticHistory renders native database rows first..first+n of a long
// conversation as the database command prints them: alternating assistant
// messages, text parts, and tool parts with sizable output.
//
//nolint:tagliatelle // The native event table names its aggregate column aggregate_id.
func syntheticHistory(root string, first, n int) []byte {
	type dbRow struct {
		ID          string `json:"id"`
		AggregateID string `json:"aggregate_id"`
		Sequence    int    `json:"seq"`
		Type        string `json:"type"`
		Data        string `json:"data"`
	}

	text := strings.Repeat("synthetic assistant text ", 16)
	output := strings.Repeat("synthetic tool output line\n", 160)
	rows := make([]dbRow, 0, n)

	for seq := first; seq < first+n; seq++ {
		message := fmt.Sprintf("msg_%08d", seq/4)

		var typ string

		var data any

		switch {
		case seq == 0:
			typ, data = "session.created.1", map[string]any{"sessionID": root, "info": map[string]any{"id": root, "directory": "/work", "title": "synthetic"}}
		case seq%4 == 1:
			typ, data = syncMessageUpdated, map[string]any{"sessionID": root, "info": map[string]any{"id": message, "sessionID": root, "role": roleAssistant, "parentID": "msg_user"}}
		case seq%4 == 2:
			typ, data = syncPartUpdated, map[string]any{"sessionID": root, "part": map[string]any{"id": fmt.Sprintf("prt_%08d", seq), "sessionID": root, "messageID": message, "type": fieldText, "text": text}}
		default:
			typ, data = syncPartUpdated, map[string]any{"sessionID": root, "part": map[string]any{"id": fmt.Sprintf("prt_%08d", seq), "sessionID": root, "messageID": message, "type": partTool, "callID": fmt.Sprintf("call_%d", seq), "tool": nativeToolBash, "state": map[string]any{"status": "completed", "input": map[string]any{"command": "ls"}, "output": output}}}
		}

		encoded, _ := json.Marshal(data)
		rows = append(rows, dbRow{ID: fmt.Sprintf("evt_%08d", seq), AggregateID: root, Sequence: seq, Type: typ, Data: string(encoded)})
	}

	encoded, _ := json.MarshalIndent(rows, "", "  ")

	return encoded
}

// BenchmarkTurnCommit measures the mirror work of one settled turn that adds
// 64 events to a 20,000-event conversation, excluding the native database
// process every read starts: decoding the turn's database output, validating
// it, capturing images, and committing it to the store.
func BenchmarkTurnCommit(b *testing.B) {
	const (
		root    = "ses_root"
		history = 20000
		turn    = 64
	)

	full, _, err := opencode.DecodeHistory(bytes.NewReader(syntheticHistory(root, 0, history)))
	require.NoError(b, err)

	s := NewAgent().newSession(sessionStart{cwd: b.TempDir()})
	s.id, s.nativeID = root, root
	s.mirror = sessionlog.NewMirror(acpcore.NewInMemorySessionStore(), root)

	synced, err := checkSync(full, root, nil)
	require.NoError(b, err)

	record := func(tail sessionlog.Tail) (any, error) {
		s.captureImages(tail.Rows)

		return s.record(), nil
	}
	require.NoError(b, s.mirror.Commit(b.Context(), sessionlog.Rows(full), record))

	output := syntheticHistory(root, history, turn)

	b.ReportAllocs()
	b.SetBytes(int64(len(output)))

	for b.Loop() {
		rows, _, err := opencode.DecodeHistory(bytes.NewReader(output))
		if err != nil {
			b.Fatal(err)
		}

		if _, err := checkSync(rows, root, synced); err != nil {
			b.Fatal(err)
		}

		// Every iteration appends the same turn, so the mirror's continuation
		// is the only source it reads.
		if err := s.mirror.Commit(b.Context(), func(sessionlog.Cursor) (sessionlog.Tail, error) {
			return sessionlog.Tail{Rows: rows}, nil
		}, record); err != nil {
			b.Fatal(err)
		}
	}
}
