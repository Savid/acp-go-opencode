package opencodeacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("decode %s field name", path)
			}

			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("duplicate field %q at %s", name, path)
			}

			seen[name] = struct{}{}

			if err := scanUniqueJSONValue(decoder, path+"."+name); err != nil {
				return err
			}
		}

		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return fmt.Errorf("close %s object: %w", path, closeErr)
		}

		if closing != json.Delim('}') {
			return fmt.Errorf("close %s object: unexpected delimiter", path)
		}
	case '[':
		index := 0

		for decoder.More() {
			if err := scanUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}

			index++
		}

		closing, closeErr := decoder.Token()
		if closeErr != nil {
			return fmt.Errorf("close %s array: %w", path, closeErr)
		}

		if closing != json.Delim(']') {
			return fmt.Errorf("close %s array: unexpected delimiter", path)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delim, path)
	}

	return nil
}

func validateStateSnapshotJSONShape(raw []byte) error {
	top, err := exactJSONObject(raw, "snapshot",
		"format", "adapterVersion", "nativeVersion", "eventSchemaVersion",
		"capturedAtUnixMilli", "restoreGeneration", "session", "graph", "events",
	)
	if err != nil {
		return err
	}

	session, err := exactJSONObject(top["session"], "snapshot.session",
		"sessionId", "nativeSessionId", "parentSessionId", "nativeParentSessionId",
		"cwd", "title", "model", "env", "extraPathDirs",
	)
	if err != nil {
		return err
	}

	if _, modelErr := exactJSONObject(session["model"], "snapshot.session.model", "providerID", "modelID", "agent"); modelErr != nil {
		return modelErr
	}

	if _, envErr := exactJSONObject(session["env"], "snapshot.session.env"); envErr != nil {
		return envErr
	}

	var graph []json.RawMessage
	if graphErr := json.Unmarshal(top["graph"], &graph); graphErr != nil {
		return fmt.Errorf("decode snapshot.graph: %w", graphErr)
	}

	for index, node := range graph {
		if _, nodeErr := exactJSONObject(node, fmt.Sprintf("snapshot.graph[%d]", index),
			"sessionId", "nativeSessionId", "parentSessionId", "nativeParentId", "sourceCwd", "permission",
		); nodeErr != nil {
			return nodeErr
		}
	}

	events, err := exactJSONObject(top["events"], "snapshot.events")
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

			event, eventErr := exactJSONObject(entry, path, "id", "aggregate_id", "seq", "type", "data")
			if eventErr != nil {
				return eventErr
			}

			if _, dataErr := exactJSONObject(event["data"], path+".data"); dataErr != nil {
				return dataErr
			}
		}
	}

	return nil
}

// exactJSONObject decodes one object with exact case-sensitive member names.
// With no allowed names it accepts dynamic keys while still requiring an
// object; duplicate keys have already been rejected by rejectDuplicateJSONFields.
func exactJSONObject(raw []byte, path string, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = fmt.Errorf("must be an object")
		}

		return nil, fmt.Errorf("decode %s: %w", path, err)
	}

	if len(allowed) == 0 {
		return fields, nil
	}

	permitted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		permitted[name] = struct{}{}
	}

	for name := range fields {
		if _, ok := permitted[name]; !ok {
			return nil, fmt.Errorf("unknown field %q at %s", name, path)
		}
	}

	return fields, nil
}
