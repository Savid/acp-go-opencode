package opencode

import (
	"encoding/json"
	"fmt"
)

const (
	syncEventTypeSessionCreated = "session.created.1"
	syncEventTypeSessionUpdated = "session.updated.1"
	syncEventFieldInfo          = "info"
	syncEventFieldMetadata      = "metadata"
)

// StripSessionCarrierFromSyncEvent removes the adapter-owned runtime carrier
// from native session events before they enter portable storage. The carrier's
// environment holds operation credentials and is never durable authority; its
// path directories are stored separately by the adapter. A restored session
// receives the current operation's complete carrier only after exact replay
// verification.
func StripSessionCarrierFromSyncEvent(event *SyncEvent) error {
	if event.Type != syncEventTypeSessionCreated && event.Type != syncEventTypeSessionUpdated {
		return nil
	}

	infoRaw, ok := event.Data[syncEventFieldInfo]
	if !ok {
		return nil
	}

	var info map[string]json.RawMessage
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		return fmt.Errorf("decode native session sync info: %w", err)
	}

	metadataRaw, ok := info[syncEventFieldMetadata]
	if !ok || string(metadataRaw) == "null" {
		return nil
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
		return fmt.Errorf("decode native session sync metadata: %w", err)
	}

	if _, ok := metadata[sessionCarrierMetadataKey]; !ok {
		return nil
	}

	delete(metadata, sessionCarrierMetadataKey)

	// Both maps came from successful JSON decoding, and the only value inserted
	// below is itself encoded JSON, so neither encoding can fail.
	encodedMetadata, _ := json.Marshal(metadata)

	info[syncEventFieldMetadata] = encodedMetadata

	encodedInfo, _ := json.Marshal(info)

	event.Data[syncEventFieldInfo] = encodedInfo

	return nil
}
