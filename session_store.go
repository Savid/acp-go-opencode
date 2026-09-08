package opencodeacp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	SessionStoreMainSubpath = ""

	// SessionStoreFormat is the one bundle shape this adapter reads and writes.
	//
	// `session.env` and `session.extraPathDirs` are required members of that
	// shape. A bundle that does not carry both is refused rather than defaulted:
	// a cold load rebinds the native session to the carrier it was captured with.
	SessionStoreFormat = "opencode-sync-events-v1"
)

type SessionStoreEntry = json.RawMessage

type SessionKey struct {
	SessionID string
	Subpath   string
}

type SessionSummary struct {
	SessionID          string
	UpdatedAtUnixMilli int64
	Cwd                string
	Title              string
	Meta               map[string]any
}

type SessionStoreReplacement struct {
	Key     SessionKey
	Entries []SessionStoreEntry
}

// SessionStore is the durable authority for every list, load, resume, and delete
// this adapter answers.
//
// Main snapshot entries persist the addressed session environment and may
// therefore contain credentials. Implementations must protect entries with
// access control, transport and at-rest safeguards, and an appropriate
// retention policy.
//
// Tombstone finality is the store's own obligation rather than the adapter's: an
// `Append` or a `Replace` addressed to a key `Delete` tombstoned writes nothing,
// clears nothing, and returns success. An implementation that leaves the rule to
// its caller resurrects a deleted session whenever a settlement races the delete
// that already answered for it.
//
// Every `Replace` is one session's, and enforcing that is the store's obligation
// too. Before writing anything, an implementation must refuse a replacement key
// whose `SessionID` differs from the addressed `main.SessionID`, and a
// `{SessionID, Subpath}` the set lists twice, with an error naming the offending
// key; a refused call leaves the store exactly as it was. The set must carry the
// addressed main key exactly once, and every other key in it is a subpath of the
// same session. A store that admits a foreign key lets one session's commit
// silently retire another session's rows.
type SessionStore interface {
	Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error
	Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error)
	Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error
	Delete(ctx context.Context, key SessionKey) error
	ListSessions(ctx context.Context) ([]SessionSummary, error)
	ListSubkeys(ctx context.Context, key SessionKey) ([]string, error)
}

type InMemorySessionStore struct {
	mu         sync.Mutex
	entries    map[SessionKey][]SessionStoreEntry
	updatedAt  map[SessionKey]int64
	tombstones map[SessionKey]int64
}

var _ SessionStore = (*InMemorySessionStore)(nil)

func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{
		entries:    make(map[SessionKey][]SessionStoreEntry),
		updatedAt:  make(map[SessionKey]int64),
		tombstones: make(map[SessionKey]int64),
	}
}

func (s *InMemorySessionStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	if len(entries) == 0 {
		return nil
	}

	if key.SessionID == "" {
		return fmt.Errorf("session id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureLocked()

	// A key tombstoned by Delete is final: the append writes nothing, clears
	// nothing, and answers success. The deleted state is already the caller's
	// answer, so there is nothing left for a later write to add to it.
	if s.isTombstonedLocked(key) {
		return nil
	}

	for _, entry := range entries {
		s.entries[key] = append(s.entries[key], cloneStoreEntry(entry))
	}

	s.updatedAt[key] = time.Now().UnixMilli()

	return nil
}

func (s *InMemorySessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isTombstonedLocked(key) {
		return nil, nil
	}

	return cloneStoreEntries(s.entries[key]), nil
}

func (s *InMemorySessionStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureLocked()

	if main.SessionID == "" {
		return fmt.Errorf("session id is required")
	}

	if main.Subpath != SessionStoreMainSubpath {
		return fmt.Errorf("main subpath must be %q", SessionStoreMainSubpath)
	}

	// Every Replace is one session's. The addressed main key names that session,
	// and a replacement key naming any other is refused before a single entry is
	// written, alongside a key the set lists twice. Both refusals happen up
	// front, so a set this store will not accept leaves it exactly as it was.
	seenReplacement := make(map[SessionKey]struct{}, len(replacements))
	mainKeys := 0

	for _, replacement := range replacements {
		// The refusal names the key it refused. A replacement set lists many keys
		// and a caller that listed one twice has to be told which, or the only way
		// to find it is to diff the set by hand.
		if _, duplicate := seenReplacement[replacement.Key]; duplicate {
			return fmt.Errorf("duplicate replacement key: session %q subpath %q",
				replacement.Key.SessionID, replacement.Key.Subpath)
		}

		seenReplacement[replacement.Key] = struct{}{}

		if replacement.Key.SessionID == "" {
			return fmt.Errorf("replacement session id is required: session %q subpath %q",
				replacement.Key.SessionID, replacement.Key.Subpath)
		}

		if replacement.Key.SessionID != main.SessionID {
			return fmt.Errorf("replacement key for a foreign session: session %q subpath %q, addressed session %q",
				replacement.Key.SessionID, replacement.Key.Subpath, main.SessionID)
		}

		if replacement.Key.Subpath == SessionStoreMainSubpath {
			mainKeys++
		}
	}

	if mainKeys != 1 {
		return fmt.Errorf("replacements must include addressed main key exactly once")
	}

	// A tombstone is final and the store is where that finality lives, not the
	// adapter above it: a replacement addressed to a deleted session writes
	// nothing, clears nothing, and answers success, because the delete already
	// gave the host its answer for that id.
	if s.isTombstonedLocked(main) {
		return nil
	}

	now := time.Now().UnixMilli()

	// The addressed session's whole subtree is retired first, so a subpath the
	// new set no longer lists cannot survive as a stale sibling of the ones it
	// does. Nothing outside that session is reachable from here: every key in
	// the set was proved to name it above.
	for candidate := range s.entries {
		if candidate.SessionID == main.SessionID {
			delete(s.entries, candidate)
			delete(s.updatedAt, candidate)
			s.tombstones[candidate] = now
		}
	}

	for _, replacement := range replacements {
		s.entries[replacement.Key] = cloneStoreEntries(replacement.Entries)
		s.updatedAt[replacement.Key] = now
		delete(s.tombstones, replacement.Key)
	}

	return nil
}

func (s *InMemorySessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("nil InMemorySessionStore")
	}

	if key.SessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureLocked()

	now := time.Now().UnixMilli()
	matched := false

	for candidate := range s.entries {
		if candidate.SessionID != key.SessionID {
			continue
		}

		if key.Subpath != SessionStoreMainSubpath && candidate.Subpath != key.Subpath {
			continue
		}

		delete(s.entries, candidate)
		delete(s.updatedAt, candidate)
		s.tombstones[candidate] = now
		matched = true
	}

	if !matched {
		s.tombstones[key] = now
	}

	if key.Subpath == SessionStoreMainSubpath {
		s.tombstones[mainSessionKey(key.SessionID)] = now
	}

	return nil
}

func (s *InMemorySessionStore) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	summaries := make([]SessionSummary, 0)

	for key, entries := range s.entries {
		if key.SessionID == "" || key.Subpath != SessionStoreMainSubpath || s.isTombstonedLocked(key) {
			continue
		}

		summary := SessionSummary{
			SessionID:          key.SessionID,
			UpdatedAtUnixMilli: s.updatedAt[key],
		}
		if len(entries) > 0 {
			summary = summaryFromStoreEntry(summary, entries[len(entries)-1])
		}

		summaries = append(summaries, summary)
	}

	slices.SortFunc(summaries, func(left, right SessionSummary) int {
		if byTime := cmp.Compare(right.UpdatedAtUnixMilli, left.UpdatedAtUnixMilli); byTime != 0 {
			return byTime
		}

		return strings.Compare(left.SessionID, right.SessionID)
	})

	return summaries, nil
}

func (s *InMemorySessionStore) ListSubkeys(ctx context.Context, key SessionKey) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s == nil {
		return nil, fmt.Errorf("nil InMemorySessionStore")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	subpaths := make([]string, 0)

	for candidate := range s.entries {
		if candidate.SessionID != key.SessionID || candidate.Subpath == SessionStoreMainSubpath || s.isTombstonedLocked(candidate) {
			continue
		}

		subpaths = append(subpaths, candidate.Subpath)
	}

	slices.Sort(subpaths)

	return subpaths, nil
}

func (s *InMemorySessionStore) ensureLocked() {
	if s.entries == nil {
		s.entries = make(map[SessionKey][]SessionStoreEntry)
	}

	if s.updatedAt == nil {
		s.updatedAt = make(map[SessionKey]int64)
	}

	if s.tombstones == nil {
		s.tombstones = make(map[SessionKey]int64)
	}
}

func (s *InMemorySessionStore) isTombstonedLocked(key SessionKey) bool {
	if s.tombstones == nil {
		return false
	}

	if _, ok := s.tombstones[key]; ok {
		return true
	}

	if key.Subpath != SessionStoreMainSubpath {
		_, ok := s.tombstones[mainSessionKey(key.SessionID)]

		return ok
	}

	return false
}

func cloneStoreEntry(entry SessionStoreEntry) SessionStoreEntry {
	return append(SessionStoreEntry(nil), entry...)
}

func cloneStoreEntries(entries []SessionStoreEntry) []SessionStoreEntry {
	if len(entries) == 0 {
		return nil
	}

	clone := make([]SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		clone = append(clone, cloneStoreEntry(entry))
	}

	return clone
}

func mainSessionKey(sessionID string) SessionKey {
	return SessionKey{SessionID: sessionID, Subpath: SessionStoreMainSubpath}
}

func summaryFromStoreEntry(summary SessionSummary, entry SessionStoreEntry) SessionSummary {
	snapshot, err := decodeStateSnapshot(entry)
	if err != nil {
		return summary
	}

	if snapshot.CapturedAtUnixMilli > 0 {
		summary.UpdatedAtUnixMilli = snapshot.CapturedAtUnixMilli
	}

	summary.Cwd = snapshot.Session.Cwd
	summary.Title = snapshot.Session.Title
	summary.Meta = map[string]any{
		opencodeMetaKey: map[string]any{
			opencodeNativeIDMetaKey: snapshot.Session.NativeSessionID,
			"stored":                true,
			jsonFieldSource:         "opencode-state",
		},
	}

	return summary
}
