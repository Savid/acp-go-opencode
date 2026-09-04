package opencodeacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	snapshotFieldAdapterVersion        = "adapterVersion"
	snapshotFieldNativeVersion         = "nativeVersion"
	snapshotFieldEventSchemaVersion    = "eventSchemaVersion"
	snapshotFieldCapturedAtUnixMilli   = "capturedAtUnixMilli"
	snapshotFieldRestoreGeneration     = "restoreGeneration"
	snapshotFieldSession               = "session"
	snapshotFieldGraph                 = "graph"
	snapshotFieldTitle                 = "title"
	snapshotFieldModel                 = "model"
	snapshotFieldEnv                   = "env"
	snapshotFieldExtraPathDirs         = "extraPathDirs"
	snapshotFieldParentSessionID       = "parentSessionId"
	snapshotFieldNativeParentID        = "nativeParentId"
	snapshotFieldNativeParentSessionID = "nativeParentSessionId"
	snapshotFieldSourceCwd             = "sourceCwd"
	snapshotEventFieldID               = "id"
	snapshotEventFieldAggregateID      = "aggregate_id"
	snapshotEventFieldSequence         = "seq"
)

// decodeStateSnapshot is the single reader for a complete persisted snapshot.
// encoding/json accepts duplicate keys and case-insensitive struct names, both
// of which make a durable restore ambiguous. The wire shape is therefore
// checked before decoding, including the typed event envelope nested beneath
// each dynamic aggregate key.
func decodeStateSnapshot(raw []byte) (stateSnapshot, error) {
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return stateSnapshot{}, err
	}

	if err := validateStateSnapshotJSONShape(raw); err != nil {
		return stateSnapshot{}, err
	}

	var snapshot stateSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return stateSnapshot{}, err
	}

	return snapshot, nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	if err := scanUniqueJSONValue(decoder, "snapshot"); err != nil {
		return err
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("snapshot carries trailing input")
		}

		return fmt.Errorf("decode snapshot trailing input: %w", err)
	}

	return nil
}

func scanUniqueJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := map[string]struct{}{}

		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("decode %s field: %w", path, err)
			}

			// encoding/json returns object member names as strings or reports a token error above.
			name, _ := nameToken.(string)

			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate field %q at %s", name, path)
			}

			seen[name] = struct{}{}

			if err := scanUniqueJSONValue(decoder, path+"."+name); err != nil {
				return err
			}
		}

		if _, closeErr := decoder.Token(); closeErr != nil {
			return fmt.Errorf("close %s object: %w", path, closeErr)
		}
	case '[':
		index := 0

		for decoder.More() {
			if err := scanUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}

			index++
		}

		if _, closeErr := decoder.Token(); closeErr != nil {
			return fmt.Errorf("close %s array: %w", path, closeErr)
		}
	}

	return nil
}

func validateStateSnapshotJSONShape(raw []byte) error {
	topMembers := []string{
		jsonFieldFormat, snapshotFieldAdapterVersion, snapshotFieldNativeVersion, snapshotFieldEventSchemaVersion,
		snapshotFieldCapturedAtUnixMilli, snapshotFieldRestoreGeneration, snapshotFieldSession, snapshotFieldGraph, jsonFieldEvents,
	}

	top, err := exactJSONObject(raw, "snapshot", topMembers, topMembers)
	if err != nil {
		return err
	}

	sessionMembers := []string{
		jsonFieldSessionID, opencodeNativeIDMetaKey, snapshotFieldParentSessionID, snapshotFieldNativeParentSessionID,
		jsonFieldCwd, snapshotFieldTitle, snapshotFieldModel, snapshotFieldEnv, snapshotFieldExtraPathDirs,
	}

	session, err := exactJSONObject(top[snapshotFieldSession], "snapshot.session", sessionMembers, []string{
		jsonFieldSessionID, opencodeNativeIDMetaKey, jsonFieldCwd, snapshotFieldTitle,
		snapshotFieldModel, snapshotFieldEnv, snapshotFieldExtraPathDirs,
	})
	if err != nil {
		return err
	}

	if _, modelErr := exactJSONObject(session[snapshotFieldModel], "snapshot.session.model",
		[]string{"providerID", "modelID", provenanceAgent}, nil); modelErr != nil {
		return modelErr
	}

	if _, envErr := exactJSONObject(session[snapshotFieldEnv], "snapshot.session.env", nil, nil); envErr != nil {
		return envErr
	}

	var graph []json.RawMessage
	if graphErr := json.Unmarshal(top[snapshotFieldGraph], &graph); graphErr != nil {
		return fmt.Errorf("decode snapshot.graph: %w", graphErr)
	}

	for index, node := range graph {
		nodeMembers := []string{
			jsonFieldSessionID, opencodeNativeIDMetaKey, snapshotFieldParentSessionID,
			snapshotFieldNativeParentID, snapshotFieldSourceCwd, metaPermissionKey,
		}
		if _, nodeErr := exactJSONObject(node, fmt.Sprintf("snapshot.graph[%d]", index), nodeMembers, []string{
			jsonFieldSessionID, opencodeNativeIDMetaKey, snapshotFieldSourceCwd, metaPermissionKey,
		}); nodeErr != nil {
			return nodeErr
		}
	}

	events, err := exactJSONObject(top[jsonFieldEvents], "snapshot.events", nil, nil)
	if err != nil {
		return err
	}

	for aggregateID, aggregate := range events {
		var entries []json.RawMessage
		if entriesErr := json.Unmarshal(aggregate, &entries); entriesErr != nil {
			return fmt.Errorf("decode snapshot.events[%q]: %w", aggregateID, entriesErr)
		}

		for index, entry := range entries {
			path := fmt.Sprintf("snapshot.events[%q][%d]", aggregateID, index)

			eventMembers := []string{
				snapshotEventFieldID, snapshotEventFieldAggregateID, snapshotEventFieldSequence, jsonFieldType, jsonFieldData,
			}

			event, eventErr := exactJSONObject(entry, path, eventMembers, eventMembers)
			if eventErr != nil {
				return eventErr
			}

			if _, dataErr := exactJSONObject(event[jsonFieldData], path+".data", nil, nil); dataErr != nil {
				return dataErr
			}
		}
	}

	return nil
}

// exactJSONObject decodes one object with exact case-sensitive member names.
// A nil allowed set accepts dynamic keys; duplicate keys have already been
// rejected by rejectDuplicateJSONFields.
func exactJSONObject(raw []byte, path string, allowed, required []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = fmt.Errorf("must be an object")
		}

		return nil, fmt.Errorf("decode %s: %w", path, err)
	}

	if allowed != nil {
		permitted := make(map[string]struct{}, len(allowed))
		for _, name := range allowed {
			permitted[name] = struct{}{}
		}

		for name := range fields {
			if _, ok := permitted[name]; !ok {
				return nil, fmt.Errorf("unknown field %q at %s", name, path)
			}
		}
	}

	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("missing required field %q at %s", name, path)
		}
	}

	return fields, nil
}
