package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/observer"
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
}

func (s *session) record() sessionRecord { return s.recordWith(nil) }

// recordWith is the session's configuration with captured artifacts recorded
// in a copy of its artifacts, leaving the session's own unchanged.
func (s *session) recordWith(captured []capturedImage) sessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	artifacts := cloneArtifacts(s.artifacts)
	order := slices.Clone(s.artifactOrder)

	for _, image := range captured {
		order = keepArtifact(artifacts, order, image.id, image.artifact)
	}

	return sessionRecord{SessionID: string(s.id), NativeSessionID: s.nativeID, Title: s.title, Cwd: s.cwd, AdditionalDirectories: slices.Clone(s.additionalDirectories), Env: maps.Clone(s.options.Env), ExtraPathDirs: slices.Clone(s.options.ExtraPathDirs), Model: s.model, Effort: s.effort, Mode: s.mode, Permission: s.options.Permission, OutputSchema: wire.CloneMap(s.options.OutputSchema), Artifacts: artifacts}
}

func (r sessionRecord) validate(id string) error {
	if !validNativeSessionID(r.NativeSessionID) || r.SessionID != id || !filepath.IsAbs(r.Cwd) {
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

// syncPosition is where a committed mirror continues: the latest committed
// sequence of each conversation in the graph, and the size of the committed
// history. It is never modified once made.
type syncPosition struct {
	sequences map[string]int64
	rows      int
	bytes     int64
}

// advance is the position after rows, whose sequences are next.
func (p syncPosition) advance(rows [][]byte, next map[string]int64) syncPosition {
	p.sequences = next
	p.rows += len(rows)

	for _, row := range rows {
		p.bytes += int64(len(row))
	}

	return p
}

// commitMirror durably publishes the native events added since the last
// commit and the current configuration. The mirror continues from the
// position it committed, so one native read returns only the new events; a
// mirror that holds nothing publishes the whole graph. An ephemeral session
// commits nothing: opencode's own database holds it until the host deletes
// it. A commit that cannot be attempted is an error: the turn it belongs to
// is not durable.
func (s *session) commitMirror(ctx context.Context, b *binding) error {
	return s.commit(ctx, b, s.nativeSource(ctx, b, false))
}

// commitConfig durably publishes a configuration change. A mirror that holds
// the session reads no native history: events opencode wrote since the last
// commit join the next one.
func (s *session) commitConfig(ctx context.Context, b *binding) error {
	return s.commit(ctx, b, s.nativeSource(ctx, b, true))
}

func (s *session) commit(ctx context.Context, b *binding, source sessionlog.Source) error {
	if s.ephemeral {
		return nil
	}

	if b == nil {
		return errors.New("session has no native binding")
	}

	// Commit may read and encode more than once; only the last read is the
	// one it commits, so its captured images are recorded after it succeeds.
	var (
		committed sessionlog.Tail
		captured  []capturedImage
	)

	ctx, finish := s.agent.observe.StartSessionStore(ctx, observer.SessionStoreCommit)
	report, err := s.mirror.Commit(ctx, source, func(tail sessionlog.Tail) (any, error) {
		committed, captured = tail, s.captureImages(tail.Rows)

		return s.recordWith(captured), nil
	})
	s.agent.observe.RecordRepublish(ctx, string(report.Republish))
	finish(err)

	if report.Recovery() {
		s.agent.log.WarnContext(ctx, "session mirror republished",
			slog.String("reason", string(report.Republish)), slog.Any("append_error", report.AppendErr))
	}

	if err != nil {
		return err
	}

	s.mu.Lock()
	for _, image := range captured {
		s.keepArtifact(image.id, image.artifact)
	}

	s.persisted = true
	s.mu.Unlock()

	if position, ok := committed.Next.Position().(syncPosition); ok {
		s.agent.log.InfoContext(ctx, "session mirror committed",
			slog.String("session_id", string(s.id)), slog.Int("rows_added", len(committed.Rows)),
			slog.Int("history_rows", position.rows), slog.Int64("history_bytes", position.bytes))
	}

	return nil
}

// nativeSource reads the native events after the position a mirror
// continues from, or the whole graph from the zero cursor. A configuration
// source continues with no events.
func (s *session) nativeSource(ctx context.Context, b *binding, configOnly bool) sessionlog.Source {
	return func(from sessionlog.Cursor) (sessionlog.Tail, error) {
		held, continues := from.Position().(syncPosition)
		if continues && configOnly {
			return sessionlog.Tail{Next: from}, nil
		}

		rows, next, err := s.readAll(ctx, b, held.sequences)
		if err != nil {
			return sessionlog.Tail{}, err
		}

		if !continues && len(rows) == 0 {
			return sessionlog.Tail{}, errors.New("native session history missing")
		}

		return sessionlog.Tail{Rows: rows, Next: sessionlog.Position(held.advance(rows, next)), Full: !continues}, nil
	}
}

// restoredSource continues the adopted stored history, the only position the
// mirror holds during the restore's commit, with the native events restore
// found after it, and reads the whole graph as nativeSource does.
func (s *session) restoredSource(ctx context.Context, b *binding, rows [][]byte, next syncPosition) sessionlog.Source {
	native := s.nativeSource(ctx, b, false)

	return func(from sessionlog.Cursor) (sessionlog.Tail, error) {
		if _, continues := from.Position().(syncPosition); continues {
			return sessionlog.Tail{Rows: rows, Next: sessionlog.Position(next)}, nil
		}

		return native(from)
	}
}

// errNativeRunning reports a conversation in the graph that is still running.
var errNativeRunning = errors.New("native session is still running")

// Reading native history waits up to nativeIdleWait, polling every
// nativeIdlePoll, for a graph that is running before or after a read.
const (
	nativeIdleWait = 5 * time.Second
	nativeIdlePoll = 100 * time.Millisecond
)

// readNative reads the graph's native events after from, page by page, with
// every conversation in the graph idle before and after the read, and returns
// the latest sequence of each. Each page goes to visit. A database query is
// one consistent snapshot; events opencode writes after its turn reports
// idle, such as a generated title or pruned tool output, reach the next
// commit. A graph found running is read again once it is idle, so a native
// run that starts while a finished turn commits delays the commit rather than
// failing it; begin runs before every attempt, so the caller can drop the
// pages of an abandoned one.
func (s *session) readNative(ctx context.Context, b *binding, from map[string]int64, begin func(), visit func(rows [][]byte) error) (map[string]int64, error) {
	started := time.Now()
	waited := false

	for {
		begin()

		next, err := s.readIdle(ctx, b, from, visit)

		switch {
		case !errors.Is(err, errNativeRunning):
			if waited && err == nil {
				s.agent.log.InfoContext(ctx, "native session graph became idle",
					slog.String("session_id", string(s.id)), slog.String("native_session_id", s.nativeID),
					slog.Duration("waited", time.Since(started)))
			}

			return next, err
		case time.Since(started) >= nativeIdleWait:
			s.agent.log.WarnContext(ctx, "native session graph stayed running",
				slog.String("session_id", string(s.id)), slog.String("native_session_id", s.nativeID),
				slog.Duration("waited", time.Since(started)))

			return nil, fmt.Errorf("native session %s graph still running after %s: %w", s.nativeID, time.Since(started).Round(time.Millisecond), err)
		case !waited:
			waited = true

			s.agent.log.InfoContext(ctx, "waiting for the native session graph to become idle",
				slog.String("session_id", string(s.id)), slog.String("native_session_id", s.nativeID),
				slog.Duration("limit", nativeIdleWait))
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(nativeIdlePoll):
		}
	}
}

func (s *session) readIdle(ctx context.Context, b *binding, from map[string]int64, visit func(rows [][]byte) error) (map[string]int64, error) {
	if err := s.requireNativeIdle(ctx, b, from); err != nil {
		return nil, err
	}

	cursors := from

	for {
		rows, more, err := opencode.ReadHistory(ctx, b.server.executable, b.server.root, b.server.environment, s.nativeID, cursors, s.agent.historyPageBytes)
		if err != nil {
			return nil, err
		}

		next, err := checkSync(rows, s.nativeID, cursors)
		if err != nil {
			return nil, err
		}

		if err := visit(rows); err != nil {
			return nil, err
		}

		cursors = next

		if !more {
			break
		}
	}

	if err := s.requireNativeIdle(ctx, b, cursors); err != nil {
		return nil, err
	}

	return cursors, nil
}

// readAll reads the graph's native events after from as readNative does.
func (s *session) readAll(ctx context.Context, b *binding, from map[string]int64) ([][]byte, map[string]int64, error) {
	var rows [][]byte

	next, err := s.readNative(ctx, b, from, func() { rows = nil }, func(page [][]byte) error {
		rows = append(rows, page...)

		return nil
	})

	return rows, next, err
}

type storedSession struct {
	rows     [][]byte
	record   sessionRecord
	position syncPosition
	found    bool
}

func (a *Agent) loadStored(ctx context.Context, id acp.SessionId) (storedSession, error) {
	ctx, finish := a.observe.StartSessionStore(ctx, observer.SessionStoreLoad)

	var (
		record    sessionRecord
		sequences map[string]int64
	)

	rows, found, err := sessionlog.Load(ctx, a.store, string(id), &record)
	if err == nil && found {
		err = record.validate(string(id))
	}

	if err == nil && found {
		sequences, err = checkSync(rows, record.NativeSessionID, nil)
	}

	if err == nil && found {
		err = validateStoredImages(rows, record)
	}

	finish(err)

	if err != nil {
		return storedSession{}, wire.RestoreRefused(ctx, a.log, vendor, id, err)
	}

	return storedSession{rows: rows, record: record, position: syncPosition{}.advance(rows, sequences), found: found}, nil
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

// restored is what restore found beyond the stored history: the native
// events after it, in an order that continues each conversation, and the
// position after them.
type restored struct {
	rows [][]byte
	next syncPosition
}

// hydrate imports the stored events native history lacks and returns the
// native events after the stored history, so the stored history stays an
// exact prefix of the restored one. Events both hold must agree byte for
// byte. Native history is compared page by page; only events the store lacks
// are kept.
func (s *session) hydrate(ctx context.Context, b *binding, stored storedSession) (restored, error) {
	s.mu.Lock()
	s.artifacts = cloneArtifacts(stored.record.Artifacts)
	s.artifactOrder = slices.Sorted(maps.Keys(s.artifacts))
	s.mu.Unlock()

	refuse := func(err error) (restored, error) {
		return restored{}, wire.RestoreRefused(ctx, s.agent.log, vendor, s.id, err)
	}

	index := make(map[syncKey]int, len(stored.rows))
	headers := make([]syncHeader, len(stored.rows))

	for position, row := range stored.rows {
		header, err := decodeHeader(row)
		if err != nil {
			return refuse(err)
		}

		headers[position] = header
		index[header.key()] = position
	}

	var (
		present []bool
		extra   [][]byte
	)

	compare := func(page [][]byte) error {
		for _, row := range page {
			header, err := decodeHeader(row)
			if err != nil {
				return err
			}

			position, held := index[header.key()]
			if !held {
				extra = append(extra, row)

				continue
			}

			if !bytes.Equal(stored.rows[position], row) {
				return errors.New("native history conflicts with mirror")
			}

			present[position] = true
		}

		return nil
	}

	nativeSequences, err := s.readNative(ctx, b, nil, func() {
		present, extra = make([]bool, len(stored.rows)), nil
	}, compare)
	if err != nil {
		return refuse(err)
	}

	if missing := missingReplays(stored.rows, headers, present); len(missing) > 0 {
		for _, events := range missing {
			if replayErr := b.client.Replay(ctx, s.cwd, events); replayErr != nil {
				return refuse(replayErr)
			}
		}

		imported := slices.Clone(present)
		known := len(extra)

		if _, err = s.readNative(ctx, b, nativeSequences, func() {
			copy(present, imported)

			extra = extra[:known]
		}, compare); err != nil {
			return refuse(err)
		}

		if slices.Contains(present, false) {
			return refuse(errors.New("native replay incomplete"))
		}
	}

	next, err := checkSync(extra, s.nativeID, stored.position.sequences)
	if err != nil {
		return refuse(err)
	}

	return restored{rows: extra, next: stored.position.advance(extra, next)}, nil
}

// missingReplays groups the stored events native history lacks into one
// replay per conversation, in sequence order, parents before their children:
// the native replay route takes one conversation's contiguous events at a
// time, and a child joins the graph through its parent.
func missingReplays(rows [][]byte, headers []syncHeader, present []bool) [][]opencode.SyncEvent {
	parents := map[string]string{}
	order := []string{}
	missing := map[string][]opencode.SyncEvent{}

	for position, header := range headers {
		if header.Type == "session.created.1" && header.Data.Info.ID == header.AggregateID {
			parents[header.AggregateID] = header.Data.Info.ParentID
		}

		if present[position] {
			continue
		}

		var event opencode.SyncEvent
		if json.Unmarshal(rows[position], &event) != nil {
			continue
		}

		if _, seen := missing[header.AggregateID]; !seen {
			order = append(order, header.AggregateID)
		}

		missing[header.AggregateID] = append(missing[header.AggregateID], event)
	}

	depth := func(aggregate string) int {
		n := 0
		for parent, ok := parents[aggregate]; ok && parent != "" && n < len(parents); parent, ok = parents[parent] {
			n++
		}

		return n
	}

	slices.SortStableFunc(order, func(a, b string) int { return depth(a) - depth(b) })

	replays := make([][]opencode.SyncEvent, 0, len(order))
	for _, aggregate := range order {
		replays = append(replays, missing[aggregate])
	}

	return replays
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
			return errNativeRunning
		}
	}

	return nil
}
