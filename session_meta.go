package opencodeacp

import (
	"encoding/json"
	"fmt"

	"github.com/coder/acp-go-sdk"
)

type sessionMeta struct {
	Model        string
	Env          map[string]string
	OutputSchema map[string]any
	Mode         string
	Permission   string
	RawMessages  rawMessageConfig
}

func sessionMetaFromLifecycle(meta map[string]any) (sessionMeta, error) {
	if err := validateLifecycleMeta(meta); err != nil {
		return sessionMeta{}, err
	}
	options, err := opencodeOptionsFromMeta(meta)
	if err != nil {
		return sessionMeta{}, err
	}
	outputSchema, _ := options.OutputSchema.(map[string]any)

	return sessionMeta{
		Model:        options.Model,
		Env:          options.Env,
		OutputSchema: outputSchema,
		Mode:         options.Mode,
		Permission:   normalizeOpenCodePermission(options.Permission),
		RawMessages:  rawMessageConfigFromMeta(meta),
	}, nil
}

type opencodeMetaOptions struct {
	Model        string
	Env          map[string]string
	OutputSchema any
	Mode         string
	Permission   string
}

func opencodeOptionsFromMeta(meta map[string]any) (opencodeMetaOptions, error) {
	opencodeMeta, _ := meta[opencodeMetaKey].(map[string]any)
	optionsMap, _ := opencodeMeta[metaOptionsKey].(map[string]any)
	if optionsMap == nil {
		return opencodeMetaOptions{}, nil
	}

	options := opencodeMetaOptions{}
	if model, _ := optionsMap[metaModelKey].(string); model != "" {
		options.Model = model
	}
	if rawEnv, ok := optionsMap[metaEnvKey]; ok {
		env, err := stringMapFromMeta(rawEnv)
		if err != nil {
			return opencodeMetaOptions{}, err
		}
		options.Env = env
	}
	if schema, ok := optionsMap[metaOutputSchemaKey]; ok {
		if err := validateSchemaObject(schema); err != nil {
			return opencodeMetaOptions{}, err
		}
		options.OutputSchema = cloneAny(schema)
	}
	if mode, _ := optionsMap[metaModeKey].(string); mode != "" {
		options.Mode = mode
	}
	if rawPermission, ok := optionsMap[metaPermissionKey]; ok {
		permission, ok := rawPermission.(string)
		if !ok {
			return opencodeMetaOptions{}, unsupportedField("_meta.opencode.options.permission")
		}
		if err := validateOpenCodePermission(permission); err != nil {
			return opencodeMetaOptions{}, err
		}
		options.Permission = normalizeOpenCodePermission(permission)
	}

	return options, nil
}

func validateLifecycleMeta(meta map[string]any) error {
	if len(meta) == 0 {
		return nil
	}
	if _, ok := meta["github.com/savid/acp-go-opencode"]; ok {
		return unsupportedField("_meta.github.com/savid/acp-go-opencode")
	}
	opencodeMeta, ok := meta[opencodeMetaKey].(map[string]any)
	if !ok {
		if _, exists := meta[opencodeMetaKey]; exists {
			return fmt.Errorf("_meta.opencode must be an object")
		}
		return nil
	}
	for key, value := range opencodeMeta {
		switch key {
		case metaOptionsKey:
			optionsMap, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("_meta.opencode.options must be an object")
			}
			for optionKey := range optionsMap {
				switch optionKey {
				case "model", "env", "outputSchema", "mode", "permission":
				default:
					return unsupportedField("_meta.opencode.options." + optionKey)
				}
			}
		case rawEventKey:
			rawEvent, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("_meta.opencode.rawEvent must be an object")
			}
			for rawKey, rawValue := range rawEvent {
				switch rawKey {
				case rawEventEnabledKey:
					if _, ok := rawValue.(bool); !ok {
						return fmt.Errorf("_meta.opencode.rawEvent.enabled must be a boolean")
					}
				default:
					return unsupportedField("_meta.opencode.rawEvent." + rawKey)
				}
			}
		default:
			return unsupportedField("_meta.opencode." + key)
		}
	}

	return nil
}

func unsupportedField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		"error": "unsupported",
		"field": path,
	})
}

func validateOpenCodePermission(permission string) error {
	switch permission {
	case "", openCodePermissionAsk, openCodePermissionAllow:
		return nil
	default:
		return unsupportedField("_meta.opencode.options.permission")
	}
}

func normalizeOpenCodePermission(permission string) string {
	if permission == "" {
		return openCodePermissionAsk
	}
	return permission
}

func validateSchemaObject(schema any) error {
	obj, ok := schema.(map[string]any)
	if !ok || len(obj) == 0 {
		return fmt.Errorf("output schema must be a non-empty JSON object")
	}
	if _, err := json.Marshal(obj); err != nil {
		return fmt.Errorf("output schema must be JSON serializable: %w", err)
	}

	return nil
}

func stringMapFromMeta(value any) (map[string]string, error) {
	switch typed := value.(type) {
	case map[string]string:
		return cloneStringMap(typed), nil
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, raw := range typed {
			str, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("_meta.opencode.options.env.%s must be a string", key)
			}
			out[key] = str
		}
		return out, nil
	default:
		return nil, fmt.Errorf("_meta.opencode.options.env must be an object")
	}
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneAny(value)
	}

	return cloned
}

func cloneAnySlice(values []any) []any {
	if values == nil {
		return nil
	}
	cloned := make([]any, len(values))
	for i, value := range values {
		cloned[i] = cloneAny(value)
	}

	return cloned
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case map[string]string:
		return cloneStringMap(typed)
	case []any:
		return cloneAnySlice(typed)
	default:
		return value
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}

	return cloned
}

func sessionResponseMeta(snapshot sessionSnapshot) map[string]any {
	opencodeMeta := map[string]any{
		opencodeNativeIDMetaKey: snapshot.idmap.NativeSessionID,
	}
	if model := joinModelValue(snapshot.providerID, snapshot.modelID); model != "" {
		opencodeMeta["model"] = model
		opencodeMeta["modelId"] = model
	}
	if snapshot.mode != "" {
		opencodeMeta["mode"] = snapshot.mode
	}

	return map[string]any{opencodeMetaKey: opencodeMeta}
}

func sessionInfoMeta(snapshot sessionSnapshot) map[string]any {
	return map[string]any{
		opencodeMetaKey: cloneAnyMap(sessionResponseMeta(snapshot)[opencodeMetaKey].(map[string]any)),
	}
}
