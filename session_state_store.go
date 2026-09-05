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

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	syncEventSchemaVersion = "1"
	// minNativeVersion is the oldest OpenCode release whose sync-event
	// surface has been validated for SessionStoreFormat. Startup fails
	// closed below it; newer releases are accepted and covered by the
	// event allowlist plus online replay verification on restore.
	minNativeVersion           = "1.18.3"
	snapshotBlockGeneration    = "generation"
	syncTypeSessionCreated     = "session.created.1"
	syncTypeSessionUpdated     = "session.updated.1"
	syncTypeMessageUpdated     = "message.updated.1"
	syncTypeMessagePartUpdated = "message.part.updated.1"
	syncFieldPart              = "part"
	syncFieldDirectory         = "directory"
	syncFieldRoot              = "root"
	parentPathSegment          = ".."
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
	Env                   map[string]string  `json:"env"`
	ExtraPathDirs         []string           `json:"extraPathDirs"`
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

	// A stored bundle is exactly one logical session's. OpenCode's native fork
	// produces an independent aggregate — the forked session owns a complete
	// copy of the history it branched from and its native parentID is unset — so
	// no session's restore needs another session's events, and a bundle that
	// carried them would go stale the moment that other session did any work.
	// Lineage is recorded as identity on the node and the carrier instead.
	graph := []*session{s}
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

		durableEnv := cloneStringMap(memberSnapshot.carrier.Env)
		if durableEnv == nil {
			durableEnv = map[string]string{}
		}

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
				Env:           durableEnv,
				ExtraPathDirs: append([]string{}, memberSnapshot.carrier.ExtraPathDirs...),
			},
			Graph: nodes, Events: events,
		}

		// The closed typed tree contains only sync-event RawMessages already validated as JSON.
		entry, _ := json.Marshal(bundle)

		if err := scanStateSnapshot(bundle, s.agent.graphSecretNeedles(graph)); err != nil {
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

	// A tombstoned session writes nothing. A replacement unlists the tombstone
	// for every key it writes, so a settlement racing the delete that already
	// succeeded would recreate the row and make a deleted session listable and
	// loadable again.
	if s.tombstoned() {
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
	incarnation := s.currentIncarnation()
	if incarnation != nil && incarnation.registry.blocked() {
		return metaPermissionKey
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeMessageIDs) > 0 {
		return snapshotBlockGeneration
	}

	return ""
}

func (a *Agent) graphSecretNeedles(graph []*session) []string {
	var needles []string

	for _, member := range graph {
		member.mu.Lock()
		needles = append(needles, member.secretNeedles...)
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

type syncEventDataSchema struct {
	allowed  []string
	required []string
}

var syncEventDataSchemas = map[string]syncEventDataSchema{
	syncTypeSessionCreated: {
		allowed: []string{syncFieldSessionID, syncFieldInfo}, required: []string{syncFieldSessionID, syncFieldInfo},
	},
	syncTypeSessionUpdated: {
		allowed: []string{syncFieldSessionID, syncFieldInfo}, required: []string{syncFieldSessionID, syncFieldInfo},
	},
	syncTypeMessageUpdated: {
		allowed: []string{syncFieldSessionID, syncFieldInfo}, required: []string{syncFieldSessionID, syncFieldInfo},
	},
	syncTypeMessagePartUpdated: {
		allowed:  []string{syncFieldSessionID, syncFieldPart, jsonFieldTime},
		required: []string{syncFieldSessionID, syncFieldPart, jsonFieldTime},
	},
}

const (
	syncFieldInfo      = "info"
	syncFieldSessionID = "sessionID"
)

func validateSyncEvent(event opencode.SyncEvent, node stateSnapshotNode) error {
	if event.ID == "" || event.AggregateID != node.NativeSessionID || event.Sequence < 0 {
		return fmt.Errorf("invalid sync event identity")
	}

	schema, ok := syncEventDataSchemas[event.Type]
	if !ok {
		return fmt.Errorf("unsupported sync event type %q", event.Type)
	}

	allowed := make(map[string]struct{}, len(schema.allowed))
	for _, field := range schema.allowed {
		allowed[field] = struct{}{}
	}

	for field := range event.Data {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unsupported field %q on sync event type %q", field, event.Type)
		}
	}

	for _, field := range schema.required {
		if _, ok := event.Data[field]; !ok {
			return fmt.Errorf("sync event %q is missing required field %q", event.ID, field)
		}
	}

	var sessionID string
	if err := json.Unmarshal(event.Data[syncFieldSessionID], &sessionID); err != nil || sessionID != node.NativeSessionID {
		return fmt.Errorf("sync event %q session identity mismatch", event.ID)
	}

	if event.Type == syncTypeMessagePartUpdated {
		if err := requireSyncEventObject(event, syncFieldPart); err != nil {
			return err
		}

		if !json.Valid(event.Data[jsonFieldTime]) {
			return fmt.Errorf("sync event %q field %q must be a finite number", event.ID, jsonFieldTime)
		}

		decoder := json.NewDecoder(bytes.NewReader(event.Data[jsonFieldTime]))
		decoder.UseNumber()

		var value any
		// json.Valid above proves this single JSON value decodes successfully.
		_ = decoder.Decode(&value)

		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("sync event %q field %q must be a finite number", event.ID, jsonFieldTime)
		}

		if _, err := number.Float64(); err != nil {
			return fmt.Errorf("sync event %q field %q must be a finite number", event.ID, jsonFieldTime)
		}

		return nil
	}

	if err := requireSyncEventObject(event, syncFieldInfo); err != nil {
		return err
	}

	return nil
}

func requireSyncEventObject(event opencode.SyncEvent, field string) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(event.Data[field], &value); err != nil || value == nil {
		return fmt.Errorf("sync event %q field %q must be an object", event.ID, field)
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

// errUnrestorableSnapshot marks a stored snapshot this adapter will not replay,
// as opposed to a host store that failed its own I/O. Only the first is this
// adapter's verdict to name on the wire, and the two reach the same call sites.
var errUnrestorableSnapshot = errors.New("opencode stored session is not restorable")

func unrestorableSnapshot(err error) error {
	return fmt.Errorf("%w: %w", errUnrestorableSnapshot, err)
}

func hydrateStateFromStore(ctx context.Context, store SessionStore, sessionID string) (idmapRecord, stateSnapshot, bool, error) {
	entries, err := store.Load(ctx, SessionKey{SessionID: sessionID, Subpath: SessionStoreMainSubpath})
	if err != nil || len(entries) == 0 {
		return idmapRecord{}, stateSnapshot{}, false, err
	}

	snapshot, err := decodeStateSnapshot(entries[len(entries)-1])
	if err != nil {
		return idmapRecord{}, stateSnapshot{}, false, unrestorableSnapshot(err)
	}

	if err := validateSyncSnapshot(sessionID, snapshot); err != nil {
		return idmapRecord{}, stateSnapshot{}, false, unrestorableSnapshot(err)
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

	if snapshot.Session.Env == nil {
		return fmt.Errorf("opencode sync manifest is missing env")
	}

	if _, err := sessionEnvFromMeta(snapshot.Session.Env); err != nil {
		return fmt.Errorf("opencode sync manifest env: %w", err)
	}

	if snapshot.Session.ExtraPathDirs == nil {
		return fmt.Errorf("opencode sync manifest is missing extraPathDirs")
	}

	if _, err := extraPathDirsFromMeta(snapshot.Session.ExtraPathDirs); err != nil {
		return fmt.Errorf("opencode sync manifest extraPathDirs: %w", err)
	}

	seenNative, err := validateSyncSnapshotGraph(snapshot)
	if err != nil {
		return err
	}

	return validateSyncSnapshotEvents(snapshot.Events, seenNative)
}

// validateSyncSnapshotGraph checks the one node a bundle carries. The graph is
// the session's own node and nothing else: it records the source cwd the events
// were captured under, the permission policy in force, and the session's fork
// lineage. Lineage is metadata — a fork's aggregate stands alone — so a node may
// name a parent no bundle contains, but it may never disagree with the carrier
// it belongs to.
func validateSyncSnapshotGraph(snapshot stateSnapshot) (map[string]stateSnapshotNode, error) {
	if len(snapshot.Graph) != 1 {
		return nil, fmt.Errorf("opencode sync graph must carry exactly one node")
	}

	node := snapshot.Graph[0]
	if node.SessionID == "" || node.NativeSessionID == "" || node.SourceCwd == "" {
		return nil, fmt.Errorf("invalid opencode sync graph node")
	}

	if (node.ParentSessionID == "") != (node.NativeParentID == "") {
		return nil, fmt.Errorf("opencode sync graph parent identity mismatch")
	}

	if node.SessionID != snapshot.Session.SessionID ||
		node.NativeSessionID != snapshot.Session.NativeSessionID ||
		node.ParentSessionID != snapshot.Session.ParentSessionID ||
		node.NativeParentID != snapshot.Session.NativeParentSessionID ||
		node.SourceCwd != snapshot.Session.Cwd {
		return nil, fmt.Errorf("selected carrier does not match opencode sync graph")
	}

	return map[string]stateSnapshotNode{node.NativeSessionID: node}, nil
}

func validateSyncSnapshotEvents(
	eventsByAggregate map[string][]opencode.SyncEvent,
	seenNative map[string]stateSnapshotNode,
) error {
	for aggregateID, events := range eventsByAggregate {
		node, ok := seenNative[aggregateID]
		if !ok || len(events) == 0 {
			return fmt.Errorf("opencode sync event aggregate is not allowlisted")
		}

		for index, event := range events {
			if event.Sequence != int64(index) {
				return fmt.Errorf("aggregate %q has non-contiguous sequence", aggregateID)
			}

			if err := validateSyncEvent(event, node); err != nil {
				return err
			}
		}
	}

	if len(eventsByAggregate) != len(seenNative) {
		return fmt.Errorf("opencode sync graph is incomplete")
	}

	return nil
}

func restoreSyncState(ctx context.Context, client opencode.Client, snapshot stateSnapshot, nativeID, targetCwd string) (opencode.NativeSession, error) {
	if err := validateSyncSnapshot(snapshot.Session.SessionID, snapshot); err != nil {
		return opencode.NativeSession{}, unrestorableSnapshot(err)
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
			return opencode.NativeSession{}, unrestorableSnapshot(err)
		}

		current, err := portableSyncEvents(existing[node.NativeSessionID])
		if err != nil {
			return opencode.NativeSession{}, err
		}

		// The destination may already hold events. Whichever side is shorter must
		// be the ordered prefix of the other: the aggregate is either behind the
		// stored generation, which replay completes, or ahead of it, which is
		// the same session's own native continuation past the capture. Anything
		// else is content this bundle did not write.
		if len(current) > 0 && !syncEventsAgree(current, expected) {
			return opencode.NativeSession{}, fmt.Errorf(
				"destination aggregate %q holds %d events that diverge from the stored generation's %d",
				node.NativeSessionID, len(current), len(expected))
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

		// The complete stored generation must be present, in order, from the
		// first event. Native events appended after the capture stay where they
		// are: they are this session's own later state, never replayed and never
		// removed.
		if len(actual) < len(expected) || !syncEventsAgree(actual, expected) {
			return opencode.NativeSession{}, fmt.Errorf("aggregate %q failed replay verification", node.NativeSessionID)
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
		if field != syncFieldDirectory && field != jsonFieldCwd && field != syncFieldRoot && field != jsonFieldPath {
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

// syncEventsAgree reports whether two event sequences describe the same
// aggregate history: the shorter is the ordered prefix of the longer. A
// destination behind the stored generation is completed by replay; one ahead of
// it holds native events appended after the capture, which is the ordinary state
// of a session the harness kept writing to — a title it settled, a timestamp it
// touched — after this adapter took its snapshot. Requiring exact equality there
// would make every stored session unrestorable the moment the harness wrote once
// more.
func syncEventsAgree(left, right []opencode.SyncEvent) bool {
	if len(left) > len(right) {
		left, right = right, left
	}

	return syncEventsEqual(left, right[:len(left)])
}

func syncEventsEqual(left, right []opencode.SyncEvent) bool {
	if len(left) != len(right) {
		return false
	}

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

func scanStateSnapshot(snapshot stateSnapshot, needles []string) error {
	snapshot.Session.Env = nil

	bundle, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}

	return scanSyncBundle(bundle, needles)
}

func newRestoreGeneration() (string, error) {
	var value [16]byte
	if _, err := restoreRandRead(value[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(value[:]), nil
}
