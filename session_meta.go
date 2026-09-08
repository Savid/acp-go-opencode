package opencodeacp

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	// metaVendorPath and its descendants are the request paths every refusal on
	// this namespace names. A malformed member is refused with the same
	// {error, field} shape as an unknown one: a host switching on `error` and
	// reading `field` must not have to fall back to prose for one of the two.
	metaVendorPath   = "_meta." + opencodeMetaKey
	metaOptionsPath  = metaVendorPath + "." + metaOptionsKey
	metaRawEventPath = metaVendorPath + "." + rawEventKey

	envOptionPath           = metaOptionsPath + "." + metaEnvKey
	extraPathDirsOptionPath = metaOptionsPath + "." + metaExtraPathDirsKey
)

type sessionMeta struct {
	Model        string
	OutputSchema map[string]any
	Mode         string
	// Effort is the native request preset a restored or forked session starts
	// with. No session-start option sets it; a host chooses one on the live
	// session through the effort config option.
	Effort           string
	Permission       string
	PermissionSet    bool
	Env              map[string]string
	EnvSet           bool
	ExtraPathDirs    []string
	ExtraPathDirsSet bool
	RawMessages      rawMessageConfig
}

// sessionMetaFromVendorOptions reads the session-start options this adapter
// carries in its own vendor namespace.
func sessionMetaFromVendorOptions(meta map[string]any) (sessionMeta, error) {
	if err := validateVendorOptionsMeta(meta); err != nil {
		return sessionMeta{}, err
	}

	options, err := opencodeOptionsFromMeta(meta)
	if err != nil {
		return sessionMeta{}, err
	}

	outputSchema, _ := options.OutputSchema.(map[string]any)

	return sessionMeta{
		Model:            options.Model,
		OutputSchema:     outputSchema,
		Mode:             options.Mode,
		Permission:       normalizeOpenCodePermission(options.Permission),
		PermissionSet:    options.PermissionSet,
		Env:              cloneStringMap(options.Env),
		EnvSet:           options.EnvSet,
		ExtraPathDirs:    append([]string(nil), options.ExtraPathDirs...),
		ExtraPathDirsSet: options.ExtraPathDirsSet,
		RawMessages:      rawMessageConfigFromMeta(meta),
	}, nil
}

type opencodeMetaOptions struct {
	Model            string
	OutputSchema     any
	Mode             string
	Permission       string
	PermissionSet    bool
	Env              map[string]string
	EnvSet           bool
	ExtraPathDirs    []string
	ExtraPathDirsSet bool
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
		options.EnvSet = true
	}

	if rawDirs, ok := optionsMap[metaExtraPathDirsKey]; ok {
		dirs, err := extraPathDirsFromMeta(rawDirs)
		if err != nil {
			return opencodeMetaOptions{}, err
		}

		options.ExtraPathDirs = dirs
		options.ExtraPathDirsSet = true
	}

	if rawModel, ok := optionsMap[metaModelKey]; ok {
		model, ok := rawModel.(string)
		if !ok {
			return opencodeMetaOptions{}, unsupportedField(metaOptionsPath + "." + metaModelKey)
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
			return opencodeMetaOptions{}, unsupportedField(metaOptionsPath + "." + metaModeKey)
		}

		options.Mode = mode
	}

	if rawPermission, ok := optionsMap[metaPermissionKey]; ok {
		permission, ok := rawPermission.(string)
		if !ok {
			return opencodeMetaOptions{}, unsupportedField(metaOptionsPath + "." + metaPermissionKey)
		}

		if err := validateOpenCodePermission(permission); err != nil {
			return opencodeMetaOptions{}, err
		}

		options.Permission = normalizeOpenCodePermission(permission)
		options.PermissionSet = true
	}

	return options, nil
}

// sessionEnvFromMeta reads the environment carried on the addressed native
// session. A host may send it as JSON or hand it over in process, so both
// shapes are accepted. Values are preserved exactly, including empty strings:
// an operation that clears a variable is asking for the empty value, not for
// the key to be dropped.
func sessionEnvFromMeta(value any) (map[string]string, error) {
	var env map[string]string

	switch typed := value.(type) {
	case map[string]string:
		env = cloneStringMap(typed)
	case map[string]any:
		env = make(map[string]string, len(typed))
		for key, raw := range typed {
			text, ok := raw.(string)
			if !ok {
				return nil, unsupportedField(envOptionPath + "." + key)
			}

			env[key] = text
		}
	default:
		return nil, unsupportedField(envOptionPath)
	}

	if err := validateSessionEnv(env, envOptionPath); err != nil {
		return nil, err
	}

	return env, nil
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

		if dir == "" || !filepath.IsAbs(dir) || strings.ContainsRune(dir, os.PathListSeparator) {
			return nil, unsupportedField(field)
		}

		dirs = append(dirs, dir)
	}

	return dirs, nil
}

// validateVendorOptionsMeta refuses any member of this adapter's own
// `_meta.opencode` namespace that it does not fix. It is the vendor-options
// validator and has nothing to do with the family lifecycle extension, whose
// negotiation and correlation values live in their own reserved literal.
func validateVendorOptionsMeta(meta map[string]any) error {
	if len(meta) == 0 {
		return nil
	}

	opencodeMeta, ok := meta[opencodeMetaKey].(map[string]any)
	if !ok {
		if _, exists := meta[opencodeMetaKey]; exists {
			return unsupportedField(metaVendorPath)
		}

		return nil
	}

	for key, value := range opencodeMeta {
		switch key {
		case metaOptionsKey:
			optionsMap, ok := value.(map[string]any)
			if !ok {
				return unsupportedField(metaOptionsPath)
			}

			for optionKey := range optionsMap {
				switch optionKey {
				case configModel, "outputSchema", configMode, metaPermissionKey, metaEnvKey, metaExtraPathDirsKey:
				default:
					return unsupportedField(metaOptionsPath + "." + optionKey)
				}
			}
		case rawEventKey:
			rawEvent, ok := value.(map[string]any)
			if !ok {
				return unsupportedField(metaRawEventPath)
			}

			for rawKey, rawValue := range rawEvent {
				switch rawKey {
				case rawEventEnabledKey:
					if _, ok := rawValue.(bool); !ok {
						return unsupportedField(metaRawEventPath + "." + rawEventEnabledKey)
					}
				default:
					return unsupportedField(metaRawEventPath + "." + rawKey)
				}
			}
		default:
			return unsupportedField(metaVendorPath + "." + key)
		}
	}

	return nil
}

func unsupportedField(path string) error {
	return unsupportedRequest(path)
}

// unsupportedRequest is the uniform refusal of one request member, typed for
// the in-process handlers that answer with the JSON-RPC error directly.
func unsupportedRequest(path string) *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: valUnsupported,
		jsonFieldField: path,
	})
}

// missingField refuses a reserved key the contract requires on this surface and
// the caller left out. It is the sibling verdict of unsupportedField and never
// substituted for it: `unsupported` always means a value that is present and
// refused, `missing` always means one that is required and absent.
func missingField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: valMissing,
		jsonFieldField: path,
	})
}

func validateOpenCodePermission(permission string) error {
	switch permission {
	case "", openCodePermissionAsk, openCodePermissionAllow, openCodePermissionDeny:
		return nil
	default:
		return unsupportedField(metaOptionsPath + "." + metaPermissionKey)
	}
}

func normalizeOpenCodePermission(permission string) string {
	if permission == "" {
		return openCodePermissionAsk
	}

	return permission
}

// validateSchemaObject refuses an output schema this adapter will not forward.
// Both refusals name the option path rather than describing the value: an
// embedded Go caller can hand over a map no JSON encoder accepts, and the host
// is owed the same {error, field} shape either way.
func validateSchemaObject(schema any) error {
	obj, ok := schema.(map[string]any)
	if !ok || len(obj) == 0 {
		return unsupportedField(outputSchemaOptionPath)
	}

	if _, err := json.Marshal(obj); err != nil {
		return unsupportedField(outputSchemaOptionPath)
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
	maps.Copy(cloned, values)

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
