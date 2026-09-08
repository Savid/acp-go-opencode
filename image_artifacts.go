package opencodeacp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	imageArtifactSubpathPrefix   = "images/"
	imageArtifactRecordVersion   = 1
	imageArtifactReferenceScheme = "opencodeacp-image-artifact://"
	// imageArtifactTTL bounds the replay window for stored image bytes.
	// Expired records are swept when a session's artifacts are loaded, and a
	// replay that needs swept bytes fails as storage_failed.
	imageArtifactTTL = 24 * time.Hour

	provenanceAgent = "agent"
	provenanceTool  = "tool"
)

var (
	imageArtifactNow = time.Now
	imageJSONMarshal = json.Marshal
)

// imageArtifactRecord is the canonical wrapper-owned copy of one emitted
// image artifact. It is the single durable owner of the decoded bytes: stored
// native sync events keep a reference, never a second copy of the base64.
type imageArtifactRecord struct {
	Version            int    `json:"version"`
	NativeID           string `json:"nativeId"`
	Provenance         string `json:"provenance"`
	Fingerprint        string `json:"fingerprint"`
	Mime               string `json:"mime"`
	DataURLPrefix      string `json:"dataUrlPrefix,omitempty"`
	Data               string `json:"data"`
	Filename           string `json:"filename,omitempty"`
	CreatedAtUnixMilli int64  `json:"createdAtUnixMilli"`
}

// nativeDataURL reconstructs the exact native data URL the artifact was
// extracted from, byte for byte.
func (r imageArtifactRecord) nativeDataURL() string {
	return r.DataURLPrefix + r.Data
}

func imageArtifactSubpath(fingerprint string) string {
	return imageArtifactSubpathPrefix + fingerprint
}

func imageFingerprint(decoded []byte) string {
	sum := sha256.Sum256(decoded)

	return hex.EncodeToString(sum[:])
}

// registerImageArtifact makes the artifact durable before its content block
// is emitted. Identical bytes register once; later identities share the
// record.
func (s *session) registerImageArtifact(ctx context.Context, identity string, record imageArtifactRecord) error {
	s.imageArtifactMu.Lock()
	defer s.imageArtifactMu.Unlock()

	s.mu.Lock()
	if s.imageArtifacts == nil {
		s.imageArtifacts = map[string]imageArtifactRecord{}
	}

	if s.imageArtifactIdentities == nil {
		s.imageArtifactIdentities = map[string]string{}
	}

	_, exists := s.imageArtifacts[record.Fingerprint]
	if exists {
		s.imageArtifactIdentities[identity] = record.Fingerprint
	}

	agent := s.agent
	id := s.id
	s.mu.Unlock()

	if exists {
		return nil
	}

	entry, err := imageJSONMarshal(record)
	if err != nil {
		return imageOutputFailure(outputReasonStorageFailed, fmt.Sprintf("encode image artifact record: %v", err), 0, 0)
	}

	storeCtx, cancel := context.WithTimeout(ctx, sessionStateReplaceTimeout)
	defer cancel()

	key := SessionKey{SessionID: string(id), Subpath: imageArtifactSubpath(record.Fingerprint)}
	if err := agent.sessionStore().Append(storeCtx, key, []SessionStoreEntry{entry}); err != nil {
		return imageOutputFailure(outputReasonStorageFailed, fmt.Sprintf("store image artifact: %v", err), 0, 0)
	}

	s.mu.Lock()
	s.imageArtifacts[record.Fingerprint] = record
	s.imageArtifactIdentities[identity] = record.Fingerprint
	s.mu.Unlock()

	return nil
}

func (s *session) imageArtifactByIdentity(identity string) (imageArtifactRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fingerprint, ok := s.imageArtifactIdentities[identity]
	if !ok {
		return imageArtifactRecord{}, false
	}

	record, ok := s.imageArtifacts[fingerprint]

	return record, ok
}

func (s *session) cloneImageArtifacts() map[string]imageArtifactRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	cloned := make(map[string]imageArtifactRecord, len(s.imageArtifacts))
	for fingerprint := range s.imageArtifacts {
		cloned[fingerprint] = s.imageArtifacts[fingerprint]
	}

	return cloned
}

func (s *session) setImageArtifacts(records map[string]imageArtifactRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.imageArtifacts = records
	s.imageArtifactIdentities = map[string]string{}

	for fingerprint := range records {
		if nativeID := records[fingerprint].NativeID; nativeID != "" {
			s.imageArtifactIdentities[nativeID] = fingerprint
		}
	}
}

// imageArtifactReplacements returns one replacement per artifact so state
// snapshots atomically carry the canonical bytes alongside the sanitized
// native events.
func imageArtifactReplacements(sessionID string, artifacts map[string]imageArtifactRecord) ([]SessionStoreReplacement, error) {
	fingerprints := make([]string, 0, len(artifacts))
	for fingerprint := range artifacts {
		fingerprints = append(fingerprints, fingerprint)
	}

	slices.Sort(fingerprints)

	replacements := make([]SessionStoreReplacement, 0, len(fingerprints))

	for _, fingerprint := range fingerprints {
		entry, err := imageJSONMarshal(artifacts[fingerprint])
		if err != nil {
			return nil, err
		}

		replacements = append(replacements, SessionStoreReplacement{
			Key:     SessionKey{SessionID: sessionID, Subpath: imageArtifactSubpath(fingerprint)},
			Entries: []SessionStoreEntry{entry},
		})
	}

	return replacements, nil
}

// loadSessionImageArtifacts loads the session's canonical image artifacts,
// sweeping expired records. Corrupt records fail closed as storage_failed.
func (a *Agent) loadSessionImageArtifacts(ctx context.Context, sessionID string) (map[string]imageArtifactRecord, error) {
	store := a.sessionStore()

	storeCtx, cancel := a.sessionStoreContext(ctx)
	defer cancel()

	subpaths, err := store.ListSubkeys(storeCtx, SessionKey{SessionID: sessionID})
	if err != nil {
		return nil, imageStoreFailure("list image artifacts", err)
	}

	now := imageArtifactNow().UnixMilli()
	records := map[string]imageArtifactRecord{}

	for _, subpath := range subpaths {
		if !strings.HasPrefix(subpath, imageArtifactSubpathPrefix) {
			continue
		}

		key := SessionKey{SessionID: sessionID, Subpath: subpath}

		entries, err := store.Load(storeCtx, key)
		if err != nil {
			return nil, imageStoreFailure("load image artifact", err)
		}

		if len(entries) == 0 {
			continue
		}

		record, err := decodeImageArtifactRecord(entries[len(entries)-1], strings.TrimPrefix(subpath, imageArtifactSubpathPrefix))
		if err != nil {
			return nil, err
		}

		if now-record.CreatedAtUnixMilli > imageArtifactTTL.Milliseconds() {
			if err := store.Delete(storeCtx, key); err != nil {
				return nil, imageStoreFailure("sweep expired image artifact", err)
			}

			continue
		}

		records[record.Fingerprint] = record
	}

	return records, nil
}

// loadAndRehydrateArtifacts loads the session's canonical image artifacts and
// restores their references inside the captured native events, so a replayed
// transcript reconstructs the original image bytes. A reference whose artifact
// was swept fails the load rather than replaying a transcript with a hole.
func (a *Agent) loadAndRehydrateArtifacts(ctx context.Context, sessionID string, events map[string][]opencode.SyncEvent) (map[string]imageArtifactRecord, error) {
	artifacts, err := a.loadSessionImageArtifacts(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if err := rehydrateSyncEventImages(events, artifacts); err != nil {
		return nil, err
	}

	return artifacts, nil
}

// imageStoreFailure keeps caller cancellation semantics: an interrupted
// store call is not a storage failure.
func imageStoreFailure(operation string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return imageOutputFailure(outputReasonStorageFailed, fmt.Sprintf("%s: %v", operation, err), 0, 0)
}

func decodeImageArtifactRecord(entry SessionStoreEntry, fingerprint string) (imageArtifactRecord, error) {
	var record imageArtifactRecord
	if err := json.Unmarshal(entry, &record); err != nil {
		return imageArtifactRecord{}, imageOutputFailure(outputReasonStorageFailed, fmt.Sprintf("decode image artifact record: %v", err), 0, 0)
	}

	decoded, err := base64.StdEncoding.DecodeString(record.Data)
	if err != nil || record.Version != imageArtifactRecordVersion || record.Fingerprint != fingerprint || imageFingerprint(decoded) != fingerprint {
		return imageArtifactRecord{}, imageOutputFailure(outputReasonStorageFailed, "image artifact record failed integrity validation", 0, 0)
	}

	return record, nil
}

// sanitizeSyncEventImages replaces emitted image data URLs inside captured
// native part events with artifact references. Replacement is surgical on the
// raw JSON bytes so an untouched event stays byte-identical and a rehydrated
// event reconstructs the original bytes exactly. Data URLs that do not match
// a registered artifact (for example prompt input images, which are native
// session state) stay verbatim.
func sanitizeSyncEventImages(events map[string][]opencode.SyncEvent, artifacts map[string]imageArtifactRecord) {
	if len(artifacts) == 0 {
		return
	}

	byURL := make(map[string]string, len(artifacts))

	for fingerprint := range artifacts {
		if record := artifacts[fingerprint]; record.DataURLPrefix != "" {
			byURL[record.nativeDataURL()] = fingerprint
		}
	}

	if len(byURL) == 0 {
		return
	}

	for _, aggregate := range events {
		for index := range aggregate {
			part, ok := aggregate[index].Data[syncFieldPart]
			if !ok {
				continue
			}

			aggregate[index].Data[syncFieldPart] = replaceDataURLs(part, byURL)
		}
	}
}

func replaceDataURLs(raw json.RawMessage, byURL map[string]string) json.RawMessage {
	marker := []byte(`"data:`)

	var out []byte

	offset := 0

	for {
		start := bytes.Index(raw[offset:], marker)
		if start < 0 {
			break
		}

		start += offset + 1

		end := bytes.IndexByte(raw[start:], '"')
		if end < 0 {
			break
		}

		candidate := raw[start : start+end]
		if fingerprint, ok := byURL[string(candidate)]; ok && !bytes.ContainsRune(candidate, '\\') {
			out = append(out, raw[:start]...)
			out = append(out, []byte(imageArtifactReferenceScheme+fingerprint)...)
			raw = raw[start+end:]
			offset = 0

			continue
		}

		offset = start + end
	}

	if out == nil {
		return raw
	}

	return append(out, raw...)
}

// rehydrateSyncEventImages restores artifact references to their original
// native data URLs before the events are replayed into a fresh native
// runtime. A reference whose artifact was swept fails the whole load as
// storage_failed rather than replaying a transcript with a hole.
func rehydrateSyncEventImages(events map[string][]opencode.SyncEvent, artifacts map[string]imageArtifactRecord) error {
	for _, aggregate := range events {
		for index := range aggregate {
			part, ok := aggregate[index].Data[syncFieldPart]
			if !ok {
				continue
			}

			restored, err := restoreDataURLs(part, artifacts)
			if err != nil {
				return err
			}

			aggregate[index].Data[syncFieldPart] = restored
		}
	}

	return nil
}

func restoreDataURLs(raw json.RawMessage, artifacts map[string]imageArtifactRecord) (json.RawMessage, error) {
	marker := []byte(`"` + imageArtifactReferenceScheme)

	var out []byte

	for {
		start := bytes.Index(raw, marker)
		if start < 0 {
			break
		}

		start++

		end := bytes.IndexByte(raw[start:], '"')
		if end < 0 {
			break
		}

		record, ok := artifacts[string(raw[start+len(imageArtifactReferenceScheme):start+end])]
		if !ok {
			return nil, imageOutputFailure(
				outputReasonStorageFailed,
				"stored image artifact is no longer available for replay",
				0, 0,
			)
		}

		out = append(out, raw[:start]...)
		out = append(out, []byte(record.nativeDataURL())...)
		raw = raw[start+end:]
	}

	if out == nil {
		return raw, nil
	}

	return append(out, raw...), nil
}
