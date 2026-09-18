package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/savid/acp-go-core/image"

	"github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/sessionlog"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// sessionRecord carries the accepted session configuration beside its native export.
type sessionRecord struct {
	SessionID             string                   `json:"sessionId"`
	NativeSessionID       string                   `json:"nativeSessionId"`
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

	return sessionRecord{SessionID: string(s.id), NativeSessionID: s.nativeID, Cwd: s.cwd, AdditionalDirectories: slices.Clone(s.additionalDirectories), Env: maps.Clone(s.options.Env), ExtraPathDirs: slices.Clone(s.options.ExtraPathDirs), Model: s.model, Effort: s.effort, Mode: s.mode, Permission: s.options.Permission, OutputSchema: wire.CloneMap(s.options.OutputSchema), Artifacts: cloneArtifacts(s.artifacts), UpdatedAtUnixMilli: time.Now().UnixMilli()}
}

func (r sessionRecord) validate(id string) error {
	if r.NativeSessionID == "" || r.SessionID != id || !filepath.IsAbs(r.Cwd) || r.UpdatedAtUnixMilli <= 0 {
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

// commitMirror replaces only a complete native snapshot and its matching
// carrier, read through the binding the caller dispatched on.
func (s *session) commitMirror(ctx context.Context, rt *binding) error {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()

	rows, err := s.snapshotRows(ctx, rt)
	if err != nil {
		return err
	}

	ctx, finish := s.agent.observe.StartSessionStore(ctx, "replace")
	err = sessionlog.Commit(ctx, s.agent.store, string(s.id), rows, s.record())
	finish(err)

	return err
}

// snapshotRows reads the whole native history of this session. A commit that
// cannot be attempted is an error: the turn it belongs to is not durable.
func (s *session) snapshotRows(ctx context.Context, rt *binding) ([][]byte, error) {
	if rt == nil {
		return nil, errors.New("session has no native binding")
	}

	rows, err := s.readSyncRows(ctx, rt)
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return nil, errors.New("native session history missing")
	}

	s.captureImages(rows)

	return rows, nil
}

type storedSession struct {
	rows   [][]byte
	record sessionRecord
	found  bool
}

func (a *Agent) loadStored(ctx context.Context, id acp.SessionId) (storedSession, error) {
	ctx, cancel := context.WithTimeout(ctx, acpcore.SessionStoreTimeout)
	defer cancel()

	ctx, finish := a.observe.StartSessionStore(ctx, "load")

	var record sessionRecord

	rows, found, err := sessionlog.Load(ctx, a.store, string(id), &record)
	if err == nil && found {
		err = record.validate(string(id))
	}

	if err == nil && found {
		_, err = decodeEvents(rows, record.NativeSessionID)
	}

	if err == nil && found {
		err = validateStoredImages(rows, record)
	}

	finish(err)

	if err != nil {
		return storedSession{}, a.restoreRefused(ctx, id, err)
	}

	return storedSession{rows: rows, record: record, found: found}, nil
}

// decodeEvents validates identities and contiguous sequences before native replay.
func decodeEvents(rows [][]byte, id string) ([]opencode.SyncEvent, error) {
	events := make([]opencode.SyncEvent, 0, len(rows))
	seq := map[string]int64{}
	seen := map[string]bool{}

	for _, row := range rows {
		var event opencode.SyncEvent
		if decodeErr := json.Unmarshal(row, &event); decodeErr != nil {
			return nil, decodeErr
		}

		if event.ID == "" || event.AggregateID == "" || event.Type == "" || event.Data == nil || seen[event.ID] {
			return nil, errors.New("invalid native sync event")
		}

		if event.Sequence != seq[event.AggregateID] {
			return nil, errors.New("native sync sequence gap")
		}

		var sessionID string
		if err := json.Unmarshal(event.Data["sessionID"], &sessionID); err != nil || sessionID != event.AggregateID {
			return nil, errors.New("native sync identity mismatch")
		}

		seen[event.ID] = true
		seq[event.AggregateID]++
		events = append(events, event)
	}

	allowed := syncGraph(events, id)
	if !allowed[id] {
		return nil, errors.New("native root session missing")
	}

	for _, event := range events {
		if !allowed[event.AggregateID] {
			return nil, errors.New("unrelated native sync aggregate")
		}
	}

	return events, nil
}
func syncGraph(events []opencode.SyncEvent, id string) map[string]bool {
	parents := map[string]string{}

	for _, event := range events {
		if event.Type != "session.created.1" {
			continue
		}

		var info opencode.NativeSession
		if json.Unmarshal(event.Data["info"], &info) == nil && info.ID == event.AggregateID {
			parents[info.ID] = info.ParentID
		}
	}

	allowed := map[string]bool{}
	if _, ok := parents[id]; !ok {
		return allowed
	}

	allowed[id] = true

	for changed := true; changed; {
		changed = false

		for child, parent := range parents {
			if !allowed[child] && allowed[parent] {
				allowed[child] = true
				changed = true
			}
		}
	}

	return allowed
}
func (s *session) readSyncRows(ctx context.Context, rt *binding) ([][]byte, error) {
	if err := s.requireNativeIdle(ctx, rt); err != nil {
		return nil, err
	}

	events, err := opencode.ReadHistory(ctx, rt.server.executable, rt.server.environment, s.nativeID, map[string]int64{})
	if err != nil {
		return nil, err
	}

	allowed := syncGraph(events, s.nativeID)
	selected := make([]opencode.SyncEvent, 0)
	cursors := map[string]int64{}

	for _, event := range events {
		if allowed[event.AggregateID] {
			selected = append(selected, event)
			if n, ok := cursors[event.AggregateID]; !ok || n < event.Sequence {
				cursors[event.AggregateID] = event.Sequence
			}
		}
	}

	next, err := opencode.ReadHistory(ctx, rt.server.executable, rt.server.environment, s.nativeID, cursors)
	if err != nil {
		return nil, err
	}

	fencedGraph := syncGraph(append(append([]opencode.SyncEvent{}, selected...), next...), s.nativeID)
	for _, event := range next {
		if fencedGraph[event.AggregateID] {
			return nil, errors.New("native history changed while snapshotting")
		}
	}

	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Sequence != selected[j].Sequence {
			return selected[i].Sequence < selected[j].Sequence
		}

		return selected[i].ID < selected[j].ID
	})

	rows := make([][]byte, 0, len(selected))
	for _, event := range selected {
		row, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}

		if event.Raw != nil {
			row = append([]byte(nil), event.Raw...)
		}

		rows = append(rows, row)
	}

	if len(rows) > 0 {
		if _, err := decodeEvents(rows, s.nativeID); err != nil {
			return nil, err
		}
	}

	if err := s.requireNativeIdle(ctx, rt); err != nil {
		return nil, err
	}

	return rows, nil
}

// hydrate replays only a missing suffix. Existing events must agree exactly.
func (s *session) hydrate(ctx context.Context, rt *binding, stored storedSession) ([][]byte, error) {
	s.mu.Lock()
	s.artifacts = cloneArtifacts(stored.record.Artifacts)
	s.mu.Unlock()

	want, err := decodeEvents(stored.rows, s.nativeID)
	if err != nil {
		return nil, s.agent.restoreRefused(ctx, s.id, err)
	}

	rows, err := s.readSyncRows(ctx, rt)
	if err != nil {
		return nil, s.agent.restoreRefused(ctx, s.id, err)
	}

	have := map[string]map[int64]opencode.SyncEvent{}

	for _, row := range rows {
		var event opencode.SyncEvent
		if decodeErr := json.Unmarshal(row, &event); decodeErr != nil {
			return nil, decodeErr
		}

		if have[event.AggregateID] == nil {
			have[event.AggregateID] = map[int64]opencode.SyncEvent{}
		}

		have[event.AggregateID][event.Sequence] = event
	}

	missing := make([]opencode.SyncEvent, 0)

	for _, event := range want {
		if existing, ok := have[event.AggregateID][event.Sequence]; ok {
			if !sameSyncEvent(event, existing) {
				return nil, s.agent.restoreRefused(ctx, s.id, errors.New("native history conflicts with mirror"))
			}
		} else {
			missing = append(missing, event)
		}
	}

	if len(missing) > 0 {
		if replayErr := rt.client.Replay(ctx, s.cwd, missing); replayErr != nil {
			return nil, s.agent.restoreRefused(ctx, s.id, replayErr)
		}
	}

	result, err := s.readSyncRows(ctx, rt)
	if err != nil {
		return nil, s.agent.restoreRefused(ctx, s.id, err)
	}

	verified, decodeErr := decodeEvents(result, s.nativeID)
	if decodeErr != nil {
		return nil, s.agent.restoreRefused(ctx, s.id, decodeErr)
	}

	committed := map[string]opencode.SyncEvent{}
	for _, event := range verified {
		committed[event.ID] = event
	}

	for _, event := range want {
		if !sameSyncEvent(event, committed[event.ID]) {
			return nil, s.agent.restoreRefused(ctx, s.id, errors.New("native replay changed event content"))
		}
	}

	if len(result) < len(stored.rows) {
		return nil, s.agent.restoreRefused(ctx, s.id, errors.New("native replay incomplete"))
	}

	return result, nil
}
func sameSyncEvent(a, b opencode.SyncEvent) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)

	var x, y any
	if json.Unmarshal(left, &x) != nil || json.Unmarshal(right, &y) != nil {
		return false
	}

	return reflect.DeepEqual(x, y)
}

func (a *Agent) restoreRefused(ctx context.Context, id acp.SessionId, err error) error {
	a.log.ErrorContext(ctx, "OpenCode session restore failed", slog.String(nativeSessionIDKey, string(id)), slog.String("reason", err.Error()))

	return wire.RestoreFailed(vendor)
}

func storedTitle(id string, rows [][]byte) string {
	title := id

	for _, row := range rows {
		var event opencode.SyncEvent
		if json.Unmarshal(row, &event) != nil {
			continue
		}

		if event.AggregateID != id || !strings.HasPrefix(event.Type, "session.") {
			continue
		}

		var info opencode.NativeSession
		if json.Unmarshal(event.Data["info"], &info) == nil && info.Title != "" {
			title = info.Title
		}
	}

	return title
}

// nativeMessages reduces the durable sync stream to the final message/part view.
func nativeMessages(rows [][]byte, id string) []opencode.NativeMessage {
	messages := map[string]*opencode.NativeMessage{}
	order := []string{}
	parts := map[string]map[string]int{}

	for _, row := range rows {
		var event opencode.SyncEvent
		if json.Unmarshal(row, &event) != nil || event.AggregateID != id {
			continue
		}

		switch event.Type {
		case "message.updated.1":
			var info opencode.NativeMessageInfo
			if json.Unmarshal(event.Data["info"], &info) != nil {
				continue
			}

			if messages[info.ID] == nil {
				messages[info.ID] = &opencode.NativeMessage{}
				order = append(order, info.ID)
				parts[info.ID] = map[string]int{}
			}

			messages[info.ID].Info = info
		case "message.part.updated.1":
			var part opencode.NativePart
			if json.Unmarshal(event.Data["part"], &part) != nil {
				continue
			}

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
			var messageID string

			_ = json.Unmarshal(event.Data["messageID"], &messageID)
			delete(messages, messageID)
		case "message.part.removed.1":
			var messageID, partID string

			_ = json.Unmarshal(event.Data["messageID"], &messageID)

			_ = json.Unmarshal(event.Data["partID"], &partID)
			if message := messages[messageID]; message != nil {
				if index, ok := parts[messageID][partID]; ok {
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

func (s *session) requireNativeIdle(ctx context.Context, rt *binding) error {
	var statuses map[string]opencode.NativeSessionStatus
	if err := rt.client.Do(ctx, s.cwd, http.MethodGet, "/session/status", nil, &statuses); err != nil {
		return err
	}

	if state := statuses[s.nativeID].Type; state != "" && state != statusIdle {
		return errors.New("native session is still running")
	}

	return nil
}
