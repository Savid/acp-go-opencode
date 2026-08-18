//nolint:tagliatelle // Native sync events use aggregate_id; replay uses aggregateID.
package opencodeacp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	syncEventSchemaVersion = "1"
	// minNativeVersion is the oldest OpenCode release whose sync-event
	// surface has been validated for SessionStoreFormat. Startup fails
	// closed below it; newer releases are accepted and covered by the
	// event allowlist plus online replay verification on restore.
	minNativeVersion        = "1.18.3"
	snapshotBlockGeneration = "generation"
	syncTypeSessionCreated  = "session.created.1"
	syncFieldPart           = "part"
	syncFieldDirectory      = "directory"
	syncFieldRoot           = "root"
	parentPathSegment       = ".."
)

// A native write that lands between the two /sync/history reads invalidates the
// generation being exported. The reads are loopback-fast (measured p50 3ms, max
// 28ms) while native writes on a working session arrive hundreds of milliseconds
// apart (measured p50 96-278ms, max 1.23s), and a settling session stops writing
// within about 0.75s of its turn going idle. Re-reading a whole generation is
// therefore near-certain to land in a quiet window: five retries spend 1.55s of
// backoff, which outlasts both the longest measured write gap and the longest
// measured post-idle tail.
const (
	stateCaptureAttempts    = 6
	stateCaptureBackoffBase = 50 * time.Millisecond
	stateCaptureBackoffCap  = 800 * time.Millisecond
)

// errGraphChangedDuringExport reports that the native graph was written while
// the generation was being read. It is the only retryable capture failure:
// every other error names a durable defect rather than a transient overlap.
var errGraphChangedDuringExport = errors.New("OpenCode graph changed during export")

var sessionStateReplaceTimeout = 60 * time.Second
var restoreRandRead = rand.Read

// stateCaptureSettleBudget caps the wall clock the retries may add to a single
// snapshot, so a session that never stops writing cannot make the snapshot
// outlive the turn or the cancellation containment it belongs to. The prompt
// path snapshots under a context detached from the turn, so without this bound
// nothing would stop the loop; the cancellation path already carries a deadline
// and keeps whichever bound expires first.
var stateCaptureSettleBudget = 5 * time.Second

var stateCaptureWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func stateCaptureBackoff(attempt int) time.Duration {
	backoff := stateCaptureBackoffBase << attempt
	if backoff <= 0 || backoff > stateCaptureBackoffCap {
		return stateCaptureBackoffCap
	}

	return backoff
}

type idmapRecord struct {
	SessionID             string `json:"sessionId"`
	NativeSessionID       string `json:"nativeSessionId"`
	ParentSessionID       string `json:"parentSessionId,omitempty"`
	NativeParentSessionID string `json:"nativeParentSessionId,omitempty"`
	Format                string `json:"format"`
	CreatedAtUnixMilli    int64  `json:"createdAtUnixMilli"`
	UpdatedAtUnixMilli    int64  `json:"updatedAtUnixMilli"`
}

type stateSnapshot struct {
	Format              string                          `json:"format"`
	AdapterVersion      string                          `json:"adapterVersion"`
	NativeVersion       string                          `json:"nativeVersion"`
	EventSchemaVersion  string                          `json:"eventSchemaVersion"`
	CapturedAtUnixMilli int64                           `json:"capturedAtUnixMilli"`
	RestoreGeneration   string                          `json:"restoreGeneration"`
	Session             stateSnapshotSession            `json:"session"`
	Graph               []stateSnapshotNode             `json:"graph"`
	Events              map[string][]opencode.SyncEvent `json:"events"`
}

type stateSnapshotSession struct {
	SessionID             string             `json:"sessionId"`
	NativeSessionID       string             `json:"nativeSessionId"`
	ParentSessionID       string             `json:"parentSessionId,omitempty"`
	NativeParentSessionID string             `json:"nativeParentSessionId,omitempty"`
	Cwd                   string             `json:"cwd"`
	Title                 string             `json:"title"`
	Model                 stateSnapshotModel `json:"model"`
	// ExtraPathDirs is the durable half of the addressed-session carrier: a
	// cold load rebinds the native session to these directories rather than to
	// whatever the reloading host happens to hold.
	//
	// The carrier environment is deliberately absent. It is where an operation
	// bearer lives, this bundle is scanned for exactly that material before it
	// is written, and a rotated bearer must come from the request that reloads
	// the session rather than from a value frozen at capture time.
	ExtraPathDirs []string `json:"extraPathDirs"`
}

type stateSnapshotModel struct {
	ProviderID string `json:"providerID,omitempty"`
	ModelID    string `json:"modelID,omitempty"`
	Agent      string `json:"agent,omitempty"`
}

type stateSnapshotNode struct {
	SessionID       string `json:"sessionId"`
	NativeSessionID string `json:"nativeSessionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	NativeParentID  string `json:"nativeParentId,omitempty"`
	SourceCwd       string `json:"sourceCwd"`
	Permission      string `json:"permission"`
}

type capturedStateSnapshot struct {
	replacements []SessionStoreReplacement
}

func (s *session) snapshotToStore(ctx context.Context) error {
	captured, err := s.captureStateSnapshot(ctx, false)
	if err != nil || len(captured.replacements) == 0 {
		return err
	}

	return s.commitStateSnapshot(ctx, captured)
}

// captureStateSnapshot reads one stable native sync generation without
// publishing it. Cancellation uses the split capture/commit path so it can
// collect the just-aborted turn while the loopback API is still online, prove
// the complete native process tree is gone, and only then perform remote store
// I/O. allowInterruptedGeneration is valid only after the native abort request
// has settled; pending permissions and elicitations remain hard blockers.
func (s *session) captureStateSnapshot(
	ctx context.Context,
	allowInterruptedGeneration bool,
) (capturedStateSnapshot, error) {
	if err := s.ensureNotPoisoned(); err != nil {
		return capturedStateSnapshot{}, err
	}

	// Cancellation retires and proves the complete shared native process tree
	// before the cancelled ACP turn settles. detachRuntime records that loss
	// before the loopback server is stopped. A later session/close must retain
	// the last committed sync-event generation: the interrupted native
	// generation is no longer an online snapshot source and reconnecting to its
	// dead loopback address can neither make that generation durable nor improve
	// the prior checkpoint.
	s.mu.Lock()
	runtimeLost := s.runtimeLostCause != ""
	s.mu.Unlock()

	if runtimeLost {
		return capturedStateSnapshot{}, nil
	}

	graph := s.agent.adoptedGraph(s)
	for _, member := range graph {
		if reason := member.snapshotBlockedReason(); reason != "" {
			if !allowInterruptedGeneration || member != s || reason != snapshotBlockGeneration {
				return capturedStateSnapshot{}, fmt.Errorf("cannot snapshot OpenCode graph while %s pending", reason)
			}
		}
	}

	allow := make(map[string]stateSnapshotNode, len(graph))
	nodes := make([]stateSnapshotNode, 0, len(graph))

	for _, member := range graph {
		snapshot := member.snapshot()
		node := stateSnapshotNode{
			SessionID: string(snapshot.id), NativeSessionID: snapshot.idmap.NativeSessionID,
			ParentSessionID: snapshot.idmap.ParentSessionID,
			NativeParentID:  snapshot.idmap.NativeParentSessionID,
			SourceCwd:       snapshot.cwd, Permission: snapshot.permission,
		}
		allow[node.NativeSessionID] = node
		nodes = append(nodes, node)
	}

	// Emitted image bytes live once, in the canonical artifact records that
	// ride this same replacement set; the captured native events keep
	// references instead of a second base64 copy.
	artifacts := unionImageArtifacts(graph)

	events, err := s.stableSyncGeneration(ctx, allow, artifacts)
	if err != nil {
		return capturedStateSnapshot{}, err
	}

	generation, err := newRestoreGeneration()
	if err != nil {
		return capturedStateSnapshot{}, err
	}

	now := time.Now().UnixMilli()
	replacements := make([]SessionStoreReplacement, 0, len(graph))

	for _, member := range graph {
		memberSnapshot := member.snapshot()
		bundle := stateSnapshot{
			Format: SessionStoreFormat, AdapterVersion: s.agent.options.AgentVersion,
			NativeVersion: s.client.NativeVersion(), EventSchemaVersion: syncEventSchemaVersion,
			CapturedAtUnixMilli: now, RestoreGeneration: generation,
			Session: stateSnapshotSession{
				SessionID: string(memberSnapshot.id), NativeSessionID: memberSnapshot.idmap.NativeSessionID,
				ParentSessionID:       memberSnapshot.idmap.ParentSessionID,
				NativeParentSessionID: memberSnapshot.idmap.NativeParentSessionID,
				Cwd:                   memberSnapshot.cwd, Title: memberSnapshot.title,
				Model:         stateSnapshotModel{ProviderID: memberSnapshot.providerID, ModelID: memberSnapshot.modelID, Agent: memberSnapshot.mode},
				ExtraPathDirs: append([]string{}, memberSnapshot.carrier.ExtraPathDirs...),
			},
			Graph: nodes, Events: events,
		}

		entry, marshalErr := json.Marshal(bundle)
		if marshalErr != nil {
			return capturedStateSnapshot{}, marshalErr
		}

		if err := scanSyncBundle(entry, s.agent.graphSecretNeedles(graph)); err != nil {
			return capturedStateSnapshot{}, err
		}

		replacements = append(replacements, SessionStoreReplacement{
			Key:     SessionKey{SessionID: string(memberSnapshot.id), Subpath: SessionStoreMainSubpath},
			Entries: []SessionStoreEntry{entry},
		})

		artifactReplacements, artifactErr := imageArtifactReplacements(string(memberSnapshot.id), artifacts)
		if artifactErr != nil {
			return capturedStateSnapshot{}, artifactErr
		}

		replacements = append(replacements, artifactReplacements...)
	}

	s.agent.restoreMu.Lock()
	ownershipErr := recordSnapshotOwnership(s.client, stateSnapshot{
		RestoreGeneration: generation,
		Graph:             nodes,
	})
	s.agent.restoreMu.Unlock()

	if ownershipErr != nil {
		return capturedStateSnapshot{}, ownershipErr
	}

	return capturedStateSnapshot{replacements: replacements}, nil
}

// stableSyncGeneration reads one native generation and proves nothing in the
// allowlisted graph was written while it was being read, repeating the whole
// read when that proof fails. A detected change never narrows the proof: every
// retry re-reads every aggregate and re-applies the same predicate over every
// event kind, so the exported generation is always one the native harness held
// still for.
func (s *session) stableSyncGeneration(
	ctx context.Context,
	allow map[string]stateSnapshotNode,
	artifacts map[string]imageArtifactRecord,
) (map[string][]opencode.SyncEvent, error) {
	deadline := time.Now().Add(stateCaptureSettleBudget)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	var changed error

	for attempt := range stateCaptureAttempts {
		events, err := s.readSyncGeneration(ctx, allow, artifacts)
		if err == nil {
			return events, nil
		}

		if !errors.Is(err, errGraphChangedDuringExport) {
			return nil, err
		}

		changed = err

		if attempt == stateCaptureAttempts-1 {
			break
		}

		backoff := stateCaptureBackoff(attempt)
		if time.Now().Add(backoff).After(deadline) {
			break
		}

		if waitErr := stateCaptureWait(ctx, backoff); waitErr != nil {
			return nil, waitErr
		}
	}

	return nil, changed
}

func (s *session) readSyncGeneration(
	ctx context.Context,
	allow map[string]stateSnapshotNode,
	artifacts map[string]imageArtifactRecord,
) (map[string][]opencode.SyncEvent, error) {
	first, err := s.client.SyncHistory(ctx, map[string]int64{})
	if err != nil {
		return nil, fmt.Errorf("capture OpenCode sync history: %w", err)
	}

	events, cursors, err := allowlistedSyncEvents(first, allow)
	if err != nil {
		return nil, err
	}

	for aggregateID, nativeEvents := range events {
		events[aggregateID], err = portableSyncEvents(nativeEvents)
		if err != nil {
			return nil, err
		}
	}

	sanitizeSyncEventImages(events, artifacts)

	second, err := s.client.SyncHistory(ctx, cursors)
	if err != nil {
		return nil, fmt.Errorf("verify OpenCode sync watermark: %w", err)
	}

	for _, event := range second {
		if _, ok := allow[event.AggregateID]; ok {
			return nil, errGraphChangedDuringExport
		}
	}

	return events, nil
}

func (s *session) commitStateSnapshot(ctx context.Context, captured capturedStateSnapshot) error {
	if len(captured.replacements) == 0 {
		return nil
	}

	storeCtx, cancel := context.WithTimeout(ctx, sessionStateReplaceTimeout)
	defer cancel()

	return s.agent.sessionStore().Replace(
		storeCtx,
		SessionKey{SessionID: string(s.id)},
		captured.replacements,
	)
}

func (s *session) snapshotBlockedReason() string {
	if s.actions.blocked() {
		return metaPermissionKey
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeMessageIDs) > 0 {
		return snapshotBlockGeneration
	}

	return ""
}

func (a *Agent) adoptedGraph(selected *session) []*session {
	a.mu.Lock()

	all := make(map[acp.SessionId]*session, len(a.sessions))
	for id, member := range a.sessions {
		all[id] = member
	}
	a.mu.Unlock()

	root := selected
	for root != nil {
		parentID := acp.SessionId(root.snapshot().idmap.ParentSessionID)

		parent := all[parentID]
		if parent == nil {
			break
		}

		root = parent
	}

	children := make(map[acp.SessionId][]*session)

	for _, member := range all {
		parent := acp.SessionId(member.snapshot().idmap.ParentSessionID)
		children[parent] = append(children[parent], member)
	}

	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *session) int {
			return strings.Compare(string(left.id), string(right.id))
		})
	}

	var graph []*session

	var visit func(*session)

	visit = func(member *session) {
		graph = append(graph, member)
		for _, child := range children[member.id] {
			visit(child)
		}
	}
	visit(root)

	return graph
}

func (a *Agent) graphSecretNeedles(graph []*session) []string {
	var needles []string

	for _, member := range graph {
		member.mu.Lock()
		needles = append(needles, member.secretNeedles...)
		// The carrier is the one environment a stored snapshot writes out, so
		// its bearer values are redacted from the same pass that redacts the
		// agent-wide ones.
		needles = append(needles, sensitiveEnvNeedles(member.carrier.Env)...)
		member.mu.Unlock()
	}

	needles = append(needles, sensitiveEnvNeedles(a.options.Env)...)

	return needles
}

// sensitiveEnvNeedles names the process-environment values a stored snapshot
// must never carry in clear text. The name decides: an environment map is
// operator- or host-supplied and carries no per-key sensitivity marking.
func sensitiveEnvNeedles(env map[string]string) []string {
	var needles []string

	for key, value := range env {
		if value == "" {
			continue
		}

		upper := strings.ToUpper(key)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") ||
			strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "AUTH") || strings.Contains(upper, "COOKIE") {
			needles = append(needles, value)
		}
	}

	return needles
}

func allowlistedSyncEvents(history []opencode.SyncEvent, allow map[string]stateSnapshotNode) (map[string][]opencode.SyncEvent, map[string]int64, error) {
	grouped := make(map[string][]opencode.SyncEvent, len(allow))
	for _, event := range history {
		node, ok := allow[event.AggregateID]
		if !ok {
			continue
		}

		if err := validateSyncEvent(event, node); err != nil {
			return nil, nil, err
		}

		grouped[event.AggregateID] = append(grouped[event.AggregateID], cloneSyncEvent(event))
	}

	cursors := make(map[string]int64, len(allow))
	for aggregateID := range allow {
		events := grouped[aggregateID]
		if len(events) == 0 {
			return nil, nil, fmt.Errorf("sync history missing aggregate %q", aggregateID)
		}

		slices.SortFunc(events, func(left, right opencode.SyncEvent) int {
			if left.Sequence < right.Sequence {
				return -1
			}

			if left.Sequence > right.Sequence {
				return 1
			}

			return strings.Compare(left.ID, right.ID)
		})

		for index, event := range events {
			if event.Sequence != int64(index) {
				return nil, nil, fmt.Errorf("aggregate %q has non-contiguous sequence", aggregateID)
			}
		}

		grouped[aggregateID] = events
		cursors[aggregateID] = events[len(events)-1].Sequence
	}

	return grouped, cursors, nil
}

var syncDataFields = map[string]map[string]struct{}{
	syncTypeSessionCreated:   {syncFieldSessionID: {}, syncFieldInfo: {}},
	"session.updated.1":      {syncFieldSessionID: {}, syncFieldInfo: {}},
	"message.updated.1":      {syncFieldSessionID: {}, syncFieldInfo: {}},
	"message.part.updated.1": {syncFieldSessionID: {}, syncFieldPart: {}, "time": {}},
}

const (
	syncFieldInfo      = "info"
	syncFieldSessionID = "sessionID"
)

func validateSyncEvent(event opencode.SyncEvent, node stateSnapshotNode) error {
	if event.ID == "" || event.AggregateID != node.NativeSessionID || event.Sequence < 0 {
		return fmt.Errorf("invalid sync event identity")
	}

	allowed, ok := syncDataFields[event.Type]
	if !ok {
		return fmt.Errorf("unsupported sync event type %q", event.Type)
	}

	for field := range event.Data {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unsupported field %q on sync event type %q", field, event.Type)
		}
	}

	var sessionID string
	if err := json.Unmarshal(event.Data[syncFieldSessionID], &sessionID); err != nil || sessionID != node.NativeSessionID {
		return fmt.Errorf("sync event %q session identity mismatch", event.ID)
	}

	return nil
}

func cloneSyncEvent(event opencode.SyncEvent) opencode.SyncEvent {
	cloned := event

	cloned.Data = make(map[string]json.RawMessage, len(event.Data))
	for key, value := range event.Data {
		cloned.Data[key] = append(json.RawMessage(nil), value...)
	}

	return cloned
}

func portableSyncEvents(events []opencode.SyncEvent) ([]opencode.SyncEvent, error) {
	portable := make([]opencode.SyncEvent, len(events))
	for index, event := range events {
		portable[index] = cloneSyncEvent(event)
		if err := opencode.StripSessionCarrierFromSyncEvent(&portable[index]); err != nil {
			return nil, fmt.Errorf("sanitize sync event %q session carrier: %w", event.ID, err)
		}
	}

	return portable, nil
}

func hydrateStateFromStore(ctx context.Context, store SessionStore, sessionID string) (idmapRecord, stateSnapshot, bool, error) {
	entries, err := store.Load(ctx, SessionKey{SessionID: sessionID, Subpath: SessionStoreMainSubpath})
	if err != nil || len(entries) == 0 {
		return idmapRecord{}, stateSnapshot{}, false, err
	}

	var snapshot stateSnapshot
	if err := json.Unmarshal(entries[len(entries)-1], &snapshot); err != nil {
		return idmapRecord{}, stateSnapshot{}, false, err
	}

	if err := validateSyncSnapshot(sessionID, snapshot); err != nil {
		return idmapRecord{}, stateSnapshot{}, false, err
	}

	idmap := idmapRecord{
		SessionID: snapshot.Session.SessionID, NativeSessionID: snapshot.Session.NativeSessionID,
		ParentSessionID:       snapshot.Session.ParentSessionID,
		NativeParentSessionID: snapshot.Session.NativeParentSessionID,
		Format:                snapshot.Format, UpdatedAtUnixMilli: snapshot.CapturedAtUnixMilli,
	}

	return idmap, snapshot, true, nil
}

func validateSyncSnapshot(sessionID string, snapshot stateSnapshot) error {
	if snapshot.Format != SessionStoreFormat || snapshot.EventSchemaVersion != syncEventSchemaVersion {
		return fmt.Errorf("unsupported opencode store format or native event schema")
	}

	if snapshot.Session.SessionID != sessionID || snapshot.Session.NativeSessionID == "" || snapshot.RestoreGeneration == "" {
		return fmt.Errorf("opencode sync manifest identity mismatch")
	}

	if snapshot.Session.ExtraPathDirs == nil {
		return fmt.Errorf("opencode sync manifest is missing extraPathDirs")
	}

	if _, err := extraPathDirsFromMeta(snapshot.Session.ExtraPathDirs); err != nil {
		return fmt.Errorf("opencode sync manifest extraPathDirs: %w", err)
	}

	seen := make(map[string]stateSnapshotNode, len(snapshot.Graph))
	for _, node := range snapshot.Graph {
		if node.SessionID == "" || node.NativeSessionID == "" || node.SourceCwd == "" {
			return fmt.Errorf("invalid opencode sync graph node")
		}

		if _, exists := seen[node.NativeSessionID]; exists {
			return fmt.Errorf("duplicate opencode sync aggregate")
		}

		seen[node.NativeSessionID] = node
	}

	if _, ok := seen[snapshot.Session.NativeSessionID]; !ok {
		return fmt.Errorf("selected aggregate is absent from opencode sync graph")
	}

	for aggregateID, events := range snapshot.Events {
		node, ok := seen[aggregateID]
		if !ok || len(events) == 0 {
			return fmt.Errorf("opencode sync event aggregate is not allowlisted")
		}

		for _, event := range events {
			if err := validateSyncEvent(event, node); err != nil {
				return err
			}
		}
	}

	if len(snapshot.Events) != len(seen) {
		return fmt.Errorf("opencode sync graph is incomplete")
	}

	return nil
}

func restoreSyncState(ctx context.Context, client opencode.Client, snapshot stateSnapshot, nativeID, targetCwd string) (opencode.NativeSession, error) {
	if err := validateSyncSnapshot(snapshot.Session.SessionID, snapshot); err != nil {
		return opencode.NativeSession{}, err
	}

	history, err := client.SyncHistory(ctx, map[string]int64{})
	if err != nil {
		return opencode.NativeSession{}, err
	}

	existing := make(map[string][]opencode.SyncEvent)
	for _, event := range history {
		existing[event.AggregateID] = append(existing[event.AggregateID], event)
	}

	if err := claimRestoreOwnership(client, snapshot, existing); err != nil {
		return opencode.NativeSession{}, err
	}

	for _, node := range snapshot.Graph {
		expected, err := rebaseSyncEvents(snapshot.Events[node.NativeSessionID], node.SourceCwd, targetCwd)
		if err != nil {
			return opencode.NativeSession{}, err
		}

		current, err := portableSyncEvents(existing[node.NativeSessionID])
		if err != nil {
			return opencode.NativeSession{}, err
		}

		if len(current) > 0 && !syncEventPrefix(current, expected) {
			return opencode.NativeSession{}, fmt.Errorf("destination aggregate %q is owned by another restore", node.NativeSessionID)
		}

		if len(existing[node.NativeSessionID]) < len(expected) {
			replay := make([]opencode.SyncReplayEvent, len(expected))
			for index, event := range expected {
				replay[index] = opencode.SyncReplayEvent(event)
			}

			replayErr := client.SyncReplay(ctx, targetCwd, replay)
			if replayErr != nil {
				return opencode.NativeSession{}, fmt.Errorf("replay aggregate %q: %w", node.NativeSessionID, replayErr)
			}
		}

		verified, err := client.SyncHistory(ctx, map[string]int64{})
		if err != nil {
			return opencode.NativeSession{}, err
		}

		var actual []opencode.SyncEvent

		for _, event := range verified {
			if event.AggregateID == node.NativeSessionID {
				actual = append(actual, event)
			}
		}

		actual, err = portableSyncEvents(actual)
		if err != nil {
			return opencode.NativeSession{}, err
		}

		if !syncEventsEqual(actual, expected) {
			return opencode.NativeSession{}, fmt.Errorf("aggregate %q failed exact replay verification", node.NativeSessionID)
		}

		if err := verifyRestoreOwnership(client, snapshot, node); err != nil {
			return opencode.NativeSession{}, err
		}
	}

	return client.GetSession(ctx, nativeID)
}

func rebaseSyncEvents(events []opencode.SyncEvent, sourceCwd, targetCwd string) ([]opencode.SyncEvent, error) {
	if filepath.Clean(sourceCwd) == filepath.Clean(targetCwd) {
		out := make([]opencode.SyncEvent, len(events))
		for index, event := range events {
			out[index] = cloneSyncEvent(event)
		}

		return out, nil
	}

	out := make([]opencode.SyncEvent, len(events))
	for index, event := range events {
		out[index] = cloneSyncEvent(event)
		for field, raw := range out[index].Data {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}

			value, err := rebasePathValues(value, "", sourceCwd, targetCwd)
			if err != nil {
				return nil, fmt.Errorf("validate sync event %q field %q paths: %w", event.ID, field, err)
			}

			// value came from json.Unmarshal above, so it is always JSON-encodable.
			encoded, _ := json.Marshal(value)

			out[index].Data[field] = encoded
		}
	}

	return out, nil
}

func rebasePathValues(value any, field, sourceCwd, targetCwd string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			rebased, err := rebasePathValues(child, key, sourceCwd, targetCwd)
			if err != nil {
				return nil, err
			}

			typed[key] = rebased
		}

		return typed, nil
	case []any:
		for index, child := range typed {
			rebased, err := rebasePathValues(child, field, sourceCwd, targetCwd)
			if err != nil {
				return nil, err
			}

			typed[index] = rebased
		}

		return typed, nil
	case string:
		if field != syncFieldDirectory && field != "cwd" && field != syncFieldRoot && field != jsonFieldPath {
			return typed, nil
		}

		if typed == "" || !filepath.IsAbs(typed) {
			return typed, nil
		}

		relative, err := filepath.Rel(sourceCwd, typed)
		if err != nil || relative == parentPathSegment || strings.HasPrefix(relative, parentPathSegment+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return nil, fmt.Errorf("absolute %s %q escapes source cwd %q", field, typed, sourceCwd)
		}

		if relative == "." {
			return targetCwd, nil
		}

		return filepath.Join(targetCwd, relative), nil
	default:
		return value, nil
	}
}

func syncEventPrefix(current, expected []opencode.SyncEvent) bool {
	if len(current) > len(expected) {
		return false
	}

	return syncEventsEqual(current, expected[:len(current)])
}

func syncEventsEqual(left, right []opencode.SyncEvent) bool {
	if len(left) != len(right) {
		return false
	}

	left = append([]opencode.SyncEvent(nil), left...)
	right = append([]opencode.SyncEvent(nil), right...)

	slices.SortFunc(left, func(a, b opencode.SyncEvent) int {
		if a.Sequence < b.Sequence {
			return -1
		}

		if a.Sequence > b.Sequence {
			return 1
		}

		return 0
	})
	slices.SortFunc(right, func(a, b opencode.SyncEvent) int {
		if a.Sequence < b.Sequence {
			return -1
		}

		if a.Sequence > b.Sequence {
			return 1
		}

		return 0
	})

	for index := range left {
		a, _ := json.Marshal(left[index])

		b, _ := json.Marshal(right[index])
		if !bytes.Equal(a, b) {
			return false
		}
	}

	return true
}

func scanSyncBundle(bundle []byte, needles []string) error {
	lower := bytes.ToLower(bundle)
	for _, forbidden := range []string{`"access_token"`, `"refresh_token"`, `"authorization"`, `"cookie"`, `"api_key"`, `"share"`} {
		if bytes.Contains(lower, []byte(forbidden)) {
			return fmt.Errorf("sync bundle contains forbidden credential/account field %q", forbidden)
		}
	}

	for _, needle := range needles {
		if needle != "" && bytes.Contains(bundle, []byte(needle)) {
			return fmt.Errorf("sync bundle contains an MCP credential")
		}
	}

	return nil
}

func newRestoreGeneration() (string, error) {
	var value [16]byte
	if _, err := restoreRandRead(value[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(value[:]), nil
}
