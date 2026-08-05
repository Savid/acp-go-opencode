package opencodeacp

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	envOptionPath           = "_meta.opencode.options." + metaEnvKey
	extraPathDirsOptionPath = "_meta.opencode.options." + metaExtraPathDirsKey
)

type sessionMeta struct {
	Model         string
	OutputSchema  map[string]any
	Mode          string
	Permission    string
	PermissionSet bool
	Env           map[string]string
	ExtraPathDirs []string
	RawMessages   rawMessageConfig
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
		Model:         options.Model,
		OutputSchema:  outputSchema,
		Mode:          options.Mode,
		Permission:    normalizeOpenCodePermission(options.Permission),
		PermissionSet: options.PermissionSet,
		Env:           options.Env,
		ExtraPathDirs: options.ExtraPathDirs,
		RawMessages:   rawMessageConfigFromMeta(meta),
	}, nil
}

type opencodeMetaOptions struct {
	Model         string
	OutputSchema  any
	Mode          string
	Permission    string
	PermissionSet bool
	Env           map[string]string
	ExtraPathDirs []string
}

func opencodeOptionsFromMeta(meta map[string]any) (opencodeMetaOptions, error) {
	opencodeMeta, _ := meta[opencodeMetaKey].(map[string]any)

	optionsMap, _ := opencodeMeta[metaOptionsKey].(map[string]any)
	if optionsMap == nil {
		return opencodeMetaOptions{}, nil
	}

	options := opencodeMetaOptions{}

	if rawEnv, ok := optionsMap[metaEnvKey]; ok {
		env, err := sessionEnvFromMeta(rawEnv)
		if err != nil {
			return opencodeMetaOptions{}, err
		}

		options.Env = env
	}

	if rawDirs, ok := optionsMap[metaExtraPathDirsKey]; ok {
		dirs, err := extraPathDirsFromMeta(rawDirs)
		if err != nil {
			return opencodeMetaOptions{}, err
		}

		options.ExtraPathDirs = dirs
	}

	if rawModel, ok := optionsMap[metaModelKey]; ok {
		model, ok := rawModel.(string)
		if !ok {
			return opencodeMetaOptions{}, unsupportedField("_meta.opencode.options." + metaModelKey)
		}

		options.Model = model
	}

	if schema, ok := optionsMap[metaOutputSchemaKey]; ok {
		if err := validateSchemaObject(schema); err != nil {
			return opencodeMetaOptions{}, err
		}

		options.OutputSchema = cloneAny(schema)
	}

	if rawMode, ok := optionsMap[metaModeKey]; ok {
		mode, ok := rawMode.(string)
		if !ok {
			return opencodeMetaOptions{}, unsupportedField("_meta.opencode.options." + metaModeKey)
		}

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
		options.PermissionSet = true
	}

	return options, nil
}

// sessionEnvFromMeta reads the per-session process environment. A host may send
// it as JSON or hand it over in process, so both shapes are accepted. PATH is
// refused here rather than merged: these entries replace whole values, and the
// additive mechanism is extraPathDirs.
func sessionEnvFromMeta(value any) (map[string]string, error) {
	var values map[string]any

	switch typed := value.(type) {
	case map[string]string:
		values = make(map[string]any, len(typed))
		for key, entry := range typed {
			values[key] = entry
		}
	case map[string]any:
		values = typed
	default:
		return nil, unsupportedField(envOptionPath)
	}

	env := make(map[string]string, len(values))

	for key, raw := range values {
		text, ok := raw.(string)
		if !ok || !validEnvName(key) {
			return nil, unsupportedField(envOptionPath + "." + key)
		}

		env[key] = text
	}

	return env, nil
}

// validEnvName refuses names a child process cannot carry, plus the one name
// this option is not allowed to own.
func validEnvName(key string) bool {
	return key != "" && key != envPathKey && !reservedOpenCodeEnvKey(key) && !strings.ContainsAny(key, "=\x00")
}

// extraPathDirsFromMeta reads the directories placed ahead of the inherited
// PATH. Relative entries are refused: the native process resolves them against
// its own working directory, not the session's.
func extraPathDirsFromMeta(value any) ([]string, error) {
	var raw []any

	switch typed := value.(type) {
	case []string:
		raw = make([]any, 0, len(typed))
		for _, entry := range typed {
			raw = append(raw, entry)
		}
	case []any:
		raw = typed
	default:
		return nil, unsupportedField(extraPathDirsOptionPath)
	}

	dirs := make([]string, 0, len(raw))

	for index, entry := range raw {
		field := fmt.Sprintf("%s[%d]", extraPathDirsOptionPath, index)

		dir, ok := entry.(string)
		if !ok {
			return nil, unsupportedField(field)
		}

		if !filepath.IsAbs(dir) {
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError: errValueAbsolutePathRequired,
				jsonFieldField: field,
			})
		}

		dirs = append(dirs, dir)
	}

	return dirs, nil
}

func validateLifecycleMeta(meta map[string]any) error {
	if len(meta) == 0 {
		return nil
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
				case configModel, "outputSchema", configMode, metaPermissionKey, metaEnvKey, metaExtraPathDirsKey:
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
		jsonFieldError: errValueUnsupported,
		jsonFieldField: path,
	})
}

// lifecycleMetaError normalizes lifecycle _meta validation failures to invalid
// params (-32602): structured request errors pass through unchanged and plain
// validation errors are wrapped so malformed _meta never surfaces as an
// internal error.
func lifecycleMetaError(err error) error {
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
}

func validateOpenCodePermission(permission string) error {
	switch permission {
	case "", openCodePermissionAsk, openCodePermissionAllow, openCodePermissionDeny:
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
		opencodeMeta[configModel] = model
		opencodeMeta["modelId"] = model
	}

	if snapshot.mode != "" {
		opencodeMeta[configMode] = snapshot.mode
	}

	return map[string]any{opencodeMetaKey: opencodeMeta}
}

func sessionInfoMeta(snapshot sessionSnapshot) map[string]any {
	opencodeMeta, _ := sessionResponseMeta(snapshot)[opencodeMetaKey].(map[string]any)

	return map[string]any{
		opencodeMetaKey: cloneAnyMap(opencodeMeta),
	}
}
