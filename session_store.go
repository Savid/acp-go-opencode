package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// sessionRecord carries the accepted session configuration beside its native export.
type sessionRecord struct {
	SessionID             string                   `json:"sessionId"`
	NativeSessionID       string                   `json:"nativeSessionId"`
	Title                 string                   `json:"title,omitempty"`
	Cwd                   string                   `json:"cwd"`
	AdditionalDirectories []string                 `json:"additionalDirectories,omitempty"`
	Env                   map[string]string        `json:"env,omitempty"`
	ExtraPathDirs         []string                 `json:"extraPathDirs,omitempty"`
	Model                 string                   `json:"model,omitempty"`
	Effort                string                   `json:"effort,omitempty"`
	Mode                  string                   `json:"mode,omitempty"`
	Permission            string                   `json:"permission,omitempty"`
	OutputSchema          map[string]any           `json:"outputSchema,omitempty"`
	Artifacts             map[string]imageArtifact `json:"artifacts,omitempty"`
	UpdatedAtUnixMilli    int64                    `json:"updatedAtUnixMilli"`
}

func (s *session) record() sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return sessionRecord{SessionID: string(s.id), NativeSessionID: s.nativeID, Title: s.title, Cwd: s.cwd, AdditionalDirectories: slices.Clone(s.additionalDirectories), Env: maps.Clone(s.options.Env), ExtraPathDirs: slices.Clone(s.options.ExtraPathDirs), Model: s.model, Effort: s.effort, Mode: s.mode, Permission: s.options.Permission, OutputSchema: wire.CloneMap(s.options.OutputSchema), Artifacts: cloneArtifacts(s.artifacts), UpdatedAtUnixMilli: time.Now().UnixMilli()}
}

func (r sessionRecord) validate(id string) error {
	if !validNativeSessionID(r.NativeSessionID) || r.SessionID != id || !filepath.IsAbs(r.Cwd) || r.UpdatedAtUnixMilli <= 0 {
		return errors.New("invalid session record")
	}

	for _, dir := range r.AdditionalDirectories {
		if !filepath.IsAbs(dir) {
			return errors.New("invalid additional directory")
		}
	}

	for _, artifact := range r.Artifacts {
		if artifact.Refusal != "" {
			if _, ok := (&image.OutputError{Reason: artifact.Refusal}).Guidance(); !ok {
				return errors.New("invalid stored image refusal")
			}

			continue
		}

		_, mime, _, refusal := image.DecodeInline(artifact.Data, image.FrameClamp)
		if refusal != nil || mime != artifact.MIME {
			return errors.New("invalid stored image artifact")
		}
	}

	_, err := parseSessionMeta(inheritCarrier(sessionMeta{}, r).Meta())
	if err != nil {
		return err
	}

	return nil
}

func validNativeSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}

	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}

// committed reports whether the store holds a generation of this session.
func (s *session) committed() bool { return s.mirror != nil && s.mirror.Committed() }

// commitMirror durably publishes the native events added since the last
// commit and the current configuration. A committed mirror continues from the
// latest sequence it holds of each conversation in the graph, so one native
// read returns only the new events; otherwise the whole graph publishes. An
// ephemeral session commits nothing: opencode's own database holds it until
// the host deletes it. A commit that cannot be attempted is an error: the
// turn it belongs to is not durable.
func (s *session) commitMirror(ctx context.Context, b *binding) error {
	return s.commit(ctx, b, false)
}

// commitConfig durably publishes a configuration change. Once the store holds
// the session it reads no native history: events opencode wrote since the
// last commit join the next one.
func (s *session) commitConfig(ctx context.Context, b *binding) error {
	return s.commit(ctx, b, true)
}

func (s *session) commit(ctx context.Context, b *binding, configOnly bool) error {
	if s.ephemeral {
		return nil
	}

	if b == nil {
		return errors.New("session has no native binding")
	}

	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	var from map[string]int64
	if s.mirror.Committed() {
		from = s.synced
	}

	var (
		rows   [][]byte
		synced = from
	)

	if !configOnly || len(from) == 0 {
		var err error
		if rows, synced, err = s.readNative(ctx, b, from); err != nil {
			return err
		}

		if len(from) == 0 && len(rows) == 0 {
			return errors.New("native session history missing")
		}

		s.captureImages(rows)
	}

	reads := 0
	source := func(sessionlog.Cursor) (sessionlog.Tail, error) {
		reads++
		if reads == 1 {
			return sessionlog.Tail{Rows: rows, Full: len(from) == 0}, nil
		}

		// The store refused the continuation, so the whole graph republishes.
		full, fullSynced, readErr := s.readNative(ctx, b, nil)
		if readErr != nil {
			return sessionlog.Tail{}, readErr
		}

		synced = fullSynced

		return sessionlog.Tail{Rows: full, Full: true}, nil
	}

	ctx, finish := s.agent.observe.StartSessionStore(ctx, "commit")
	err := s.mirror.Commit(ctx, source, s.record())
	finish(err)

	if err == nil {
		s.synced = synced
	}

	return err
}

// commitRows publishes a restored history whole with the current
// configuration.
func (s *session) commitRows(ctx context.Context, rows [][]byte, synced map[string]int64) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	ctx, finish := s.agent.observe.StartSessionStore(ctx, "commit")
	err := s.mirror.Commit(ctx, sessionlog.Rows(rows), s.record())
	finish(err)

	if err == nil {
		s.synced = synced
	}

	return err
}

// readNative reads the graph's native events after from with the graph idle
// before and after the read, and returns them with the latest sequence of
// each conversation. One database query is one consistent snapshot; events
// opencode writes after its turn reports idle, such as a generated title or
// pruned tool output, reach the next commit.
func (s *session) readNative(ctx context.Context, b *binding, from map[string]int64) ([][]byte, map[string]int64, error) {
	if err := s.requireNativeIdle(ctx, b, from); err != nil {
		return nil, nil, err
	}

	rows, err := opencode.ReadHistory(ctx, b.server.executable, b.server.root, b.server.environment, s.nativeID, from)
	if err != nil {
		return nil, nil, err
	}

	synced, err := checkSync(rows, s.nativeID, from)
	if err != nil {
		return nil, nil, err
	}

	if err := s.requireNativeIdle(ctx, b, synced); err != nil {
		return nil, nil, err
	}

	return rows, synced, nil
}

type storedSession struct {
	rows   [][]byte
	record sessionRecord
	found  bool
}

func (a *Agent) loadStored(ctx context.Context, id acp.SessionId) (storedSession, error) {
	ctx, finish := a.observe.StartSessionStore(ctx, "load")

	var record sessionRecord

	rows, found, err := sessionlog.Load(ctx, a.store, string(id), &record)
	if err == nil && found {
		err = record.validate(string(id))
	}

	if err == nil && found {
		_, err = checkSync(rows, record.NativeSessionID, nil)
	}

	if err == nil && found {
		err = validateStoredImages(rows, record)
	}

	finish(err)

	if err != nil {
		return storedSession{}, wire.RestoreRefused(ctx, a.log, vendor, id, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

// syncKey identifies one native sync event by conversation and sequence.
type syncKey struct {
	aggregate string
	sequence  int64
}

// syncHeader is the identity of one mirror row.
//
//nolint:tagliatelle // The native event table names its aggregate column aggregate_id.
type syncHeader struct {
	ID          string `json:"id"`
	AggregateID string `json:"aggregate_id"`
	Sequence    int64  `json:"seq"`
	Type        string `json:"type"`
	Data        struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID       string `json:"id"`
			ParentID string `json:"parentID"`
		} `json:"info"`
	} `json:"data"`
}

func (h syncHeader) key() syncKey { return syncKey{aggregate: h.AggregateID, sequence: h.Sequence} }

func decodeHeader(row []byte) (syncHeader, error) {
	var header syncHeader
	if err := json.Unmarshal(row, &header); err != nil {
		return syncHeader{}, err
	}

	if header.ID == "" || header.AggregateID == "" || header.Type == "" {
		return syncHeader{}, errors.New("invalid native sync event")
	}

	if header.Data.SessionID != header.AggregateID {
		return syncHeader{}, errors.New("native sync identity mismatch")
	}

	return header, nil
}

// checkSync validates rows as the continuation of from, the latest sequence
// of each conversation already held, and returns the sequences after rows.
// Every conversation continues without a gap from its held sequence, or from
// zero when it is new; every event names its own conversation; and every
// conversation belongs to root's graph, joining it through its creation
// event's parent. An empty from requires root's creation among nonempty rows.
func checkSync(rows [][]byte, root string, from map[string]int64) (map[string]int64, error) {
	synced := maps.Clone(from)
	if synced == nil {
		synced = map[string]int64{}
	}

	if len(rows) == 0 {
		return synced, nil
	}

	allowed := map[string]bool{}
	for aggregate := range from {
		allowed[aggregate] = true
	}

	parents := map[string]string{}
	seen := make(map[string]bool, len(rows))
	aggregates := make([]string, 0, len(rows))

	for _, row := range rows {
		header, err := decodeHeader(row)
		if err != nil {
			return nil, err
		}

		if seen[header.ID] {
			return nil, errors.New("invalid native sync event")
		}

		seen[header.ID] = true

		expected := int64(0)
		if last, held := synced[header.AggregateID]; held {
			expected = last + 1
		}

		if header.Sequence != expected {
			return nil, errors.New("native sync sequence gap")
		}

		synced[header.AggregateID] = header.Sequence
		aggregates = append(aggregates, header.AggregateID)

		if header.Type == "session.created.1" && header.Data.Info.ID == header.AggregateID {
			parents[header.AggregateID] = header.Data.Info.ParentID
		}
	}

	if len(from) == 0 {
		if _, created := parents[root]; !created {
			return nil, errors.New("native root session missing")
		}

		allowed[root] = true
	}

	for changed := true; changed; {
		changed = false

		for child, parent := range parents {
			if !allowed[child] && allowed[parent] {
				allowed[child] = true
				changed = true
			}
		}
	}

	for _, aggregate := range aggregates {
		if !allowed[aggregate] {
			return nil, errors.New("unrelated native sync aggregate")
		}
	}

	return synced, nil
}

// hydrate imports the stored events native history lacks and returns the
// restored history: the stored rows, then the newer native events, so the
// stored history stays an exact prefix. Events both hold must agree.
func (s *session) hydrate(ctx context.Context, b *binding, stored storedSession) ([][]byte, map[string]int64, error) {
	s.mu.Lock()
	s.artifacts = cloneArtifacts(stored.record.Artifacts)
	s.artifactOrder = slices.Sorted(maps.Keys(s.artifacts))
	s.mu.Unlock()

	refuse := func(err error) ([][]byte, map[string]int64, error) {
		return nil, nil, wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
	}

	native, nativeSynced, err := s.readNative(ctx, b, nil)
	if err != nil {
		return refuse(err)
	}

	have, err := indexRows(native)
	if err != nil {
		return refuse(err)
	}

	held := make(map[syncKey]bool, len(stored.rows))

	var (
		missing     []opencode.SyncEvent
		missingRows [][]byte
	)

	for _, row := range stored.rows {
		header, decodeErr := decodeHeader(row)
		if decodeErr != nil {
			return refuse(decodeErr)
		}

		held[header.key()] = true

		existing, ok := have[header.key()]
		if !ok {
			var event opencode.SyncEvent
			if decodeErr := json.Unmarshal(row, &event); decodeErr != nil {
				return refuse(decodeErr)
			}

			missing = append(missing, event)
			missingRows = append(missingRows, row)

			continue
		}

		if !bytes.Equal(row, existing) {
			return refuse(errors.New("native history conflicts with mirror"))
		}
	}

	if len(missing) > 0 {
		if replayErr := b.client.Replay(ctx, s.cwd, missing); replayErr != nil {
			return refuse(replayErr)
		}

		late, _, lateErr := s.readNative(ctx, b, nativeSynced)
		if lateErr != nil {
			return refuse(lateErr)
		}

		imported, indexErr := indexRows(late)
		if indexErr != nil {
			return refuse(indexErr)
		}

		for index, event := range missing {
			row, ok := imported[syncKey{aggregate: event.AggregateID, sequence: event.Sequence}]
			if !ok {
				return refuse(errors.New("native replay incomplete"))
			}

			if !bytes.Equal(missingRows[index], row) {
				return refuse(errors.New("native replay changed event content"))
			}
		}

		native = append(native, late...)
	}

	rows := slices.Clip(slices.Clone(stored.rows))

	for _, row := range native {
		header, decodeErr := decodeHeader(row)
		if decodeErr != nil {
			return refuse(decodeErr)
		}

		if !held[header.key()] {
			rows = append(rows, row)
		}
	}

	synced, err := checkSync(rows, s.nativeID, nil)
	if err != nil {
		return refuse(err)
	}

	return rows, synced, nil
}

// indexRows maps each row to its sync identity.
func indexRows(rows [][]byte) (map[syncKey][]byte, error) {
	index := make(map[syncKey][]byte, len(rows))

	for _, row := range rows {
		header, err := decodeHeader(row)
		if err != nil {
			return nil, err
		}

		index[header.key()] = row
	}

	return index, nil
}

// messageEvent is the message and part view of one mirror row.
//
//nolint:tagliatelle // The native event table names its aggregate column aggregate_id; event data spells ID in uppercase.
type messageEvent struct {
	AggregateID string `json:"aggregate_id"`
	Type        string `json:"type"`
	Data        struct {
		Info      opencode.NativeMessageInfo `json:"info"`
		Part      opencode.NativePart        `json:"part"`
		MessageID string                     `json:"messageID"`
		PartID    string                     `json:"partID"`
	} `json:"data"`
}

const (
	syncMessageUpdated = "message.updated.1"
	syncPartUpdated    = "message.part.updated.1"
)

// nativeMessages reduces the durable sync stream to the final message/part view.
func nativeMessages(rows [][]byte, id string) []opencode.NativeMessage {
	messages := map[string]*opencode.NativeMessage{}
	order := []string{}
	parts := map[string]map[string]int{}

	for _, row := range rows {
		var event messageEvent
		if json.Unmarshal(row, &event) != nil || event.AggregateID != id {
			continue
		}

		switch event.Type {
		case syncMessageUpdated:
			info := event.Data.Info
			if messages[info.ID] == nil {
				messages[info.ID] = &opencode.NativeMessage{}
				order = append(order, info.ID)
				parts[info.ID] = map[string]int{}
			}

			messages[info.ID].Info = info
		case syncPartUpdated:
			part := event.Data.Part

			message := messages[part.MessageID]
			if message == nil {
				continue
			}

			if index, ok := parts[part.MessageID][part.ID]; ok {
				message.Parts[index] = part
			} else {
				parts[part.MessageID][part.ID] = len(message.Parts)
				message.Parts = append(message.Parts, part)
			}
		case "message.removed.1":
			delete(messages, event.Data.MessageID)
		case "message.part.removed.1":
			if message := messages[event.Data.MessageID]; message != nil {
				if index, ok := parts[event.Data.MessageID][event.Data.PartID]; ok {
					message.Parts[index] = opencode.NativePart{}
				}
			}
		}
	}

	result := make([]opencode.NativeMessage, 0, len(order))
	for _, id := range order {
		if message := messages[id]; message != nil {
			result = append(result, *message)
		}
	}

	return result
}

func (s *session) replay(ctx context.Context, rows [][]byte) error {
	c := &cycle{}

	messages := nativeMessages(rows, s.nativeID)
	for index := range messages {
		message := &messages[index]
		if message.Info.Role == roleUser {
			for index := range message.Parts {
				part := &message.Parts[index]

				switch part.Type {
				case fieldText:
					if err := s.emit(ctx, acp.UpdateUserMessageText(part.Text)); err != nil {
						return err
					}
				case partFile:
					for _, block := range s.outputFile(opencode.NativeAttachment{ID: part.ID, Mime: part.Mime, URL: part.URL, Filename: part.Filename}, nil) {
						if err := s.emit(ctx, acp.UpdateUserMessage(block)); err != nil {
							return err
						}
					}
				}
			}

			continue
		}

		if err := s.projectMessage(ctx, c, *message); err != nil {
			return err
		}
	}

	return nil
}

// requireNativeIdle refuses while the root session or any session in graph is
// running. The native status route lists only sessions that are not idle.
func (s *session) requireNativeIdle(ctx context.Context, b *binding, graph map[string]int64) error {
	var statuses map[string]opencode.NativeSessionStatus
	if err := b.client.Do(ctx, s.cwd, http.MethodGet, "/session/status", nil, &statuses); err != nil {
		return err
	}

	for id, status := range statuses {
		_, member := graph[id]
		if (id == s.nativeID || member) && status.Type != "" && status.Type != statusIdle {
			return errors.New("native session is still running")
		}
	}

	return nil
}
