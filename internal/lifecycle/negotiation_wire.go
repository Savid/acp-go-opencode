package lifecycle

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PreserveRequestMeta removes the owned value from SDK decoding and returns it
// unchanged for validation at the method's existing semantic boundary. Foreign
// metadata retains the SDK's behavior; this does not validate a request.
func PreserveRequestMeta(params json.RawMessage) (json.RawMessage, any, bool) {
	var retained any

	present, metaCount := false, 0
	sanitized := rewriteMetadataObject(params, func(key string, raw json.RawMessage) json.RawMessage {
		if !strings.EqualFold(key, "_meta") {
			return raw
		}

		metaCount++

		return rewriteMetadataObject(raw, func(namespace string, value json.RawMessage) json.RawMessage {
			if namespace != MetaKey {
				return value
			}

			if present {
				retained = paramError()
			} else {
				retained = value
			}

			present = true

			return json.RawMessage("null")
		})
	})

	if metaCount > 1 && present {
		retained = paramError()
	}

	return sanitized, retained, present
}

// Keep member order and duplicate foreign fields intact while removing only
// the owned value from SDK numeric decoding.
func rewriteMetadataObject(raw json.RawMessage, rewrite func(string, json.RawMessage) json.RawMessage) json.RawMessage {
	if !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return raw
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	_, _ = decoder.Token()
	out := []byte{'{'}

	for decoder.More() {
		token, _ := decoder.Token()
		key, _ := token.(string)

		var value json.RawMessage

		_ = decoder.Decode(&value)

		if len(out) > 1 {
			out = append(out, ',')
		}

		name, _ := json.Marshal(key)
		out = append(out, name...)
		out = append(out, ':')
		out = append(out, rewrite(key, value)...)
	}

	return append(out, '}')
}

// negotiationFields reads only an owned object. Raw wire values retain duplicate
// members and number spellings; direct Go callers keep their map representation.
func negotiationFields(raw any, path ...string) (map[string]any, *ParamError) {
	if refusal, ok := raw.(*ParamError); ok {
		return nil, refusal
	}

	if fields, ok := raw.(map[string]any); ok {
		return fields, nil
	}

	encoded, ok := raw.(json.RawMessage)
	if !ok {
		return nil, paramError(path...)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()

	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, paramError(path...)
	}

	fields := make(map[string]any)

	for decoder.More() {
		token, _ := decoder.Token()
		key, _ := token.(string)

		memberPath := append(append([]string(nil), path...), key)
		if _, duplicate := fields[key]; duplicate {
			return nil, paramError(memberPath...)
		}

		if key == fieldSubmission && len(path) == 0 {
			var submission json.RawMessage
			if decoder.Decode(&submission) != nil {
				return nil, paramError(memberPath...)
			}

			fields[key] = submission
		} else {
			var value any
			if decoder.Decode(&value) != nil {
				return nil, paramError(memberPath...)
			}

			fields[key] = value
		}
	}

	if _, err := decoder.Token(); err != nil {
		return nil, paramError(path...)
	}

	return fields, nil
}
