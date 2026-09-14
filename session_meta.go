package opencodeacp

import (
	"errors"
	"fmt"
	"slices"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	metaOptionsKey       = "options"
	metaRawEventKey      = "rawEvent"
	metaModelKey         = "model"
	metaEnvKey           = "env"
	metaExtraPathDirsKey = "extraPathDirs"
	metaOutputSchemaKey  = "outputSchema"
	metaEffortKey        = "effort"
	metaEnabledKey       = "enabled"
	metaModeKey          = "mode"
	metaPermissionKey    = "permission"
)

// OpenCodeOptions is the per-session options struct carried at _meta.opencode.options.
type OpenCodeOptions struct {
	// Mode selects a native agent.
	Mode string `json:"mode,omitempty"`
	// Permission selects ask, allow, or deny for native tools.
	Permission string `json:"permission,omitempty"`
	// Model selects the opencode model for this session as "provider/id".
	Model string `json:"model,omitempty"`
	// Env overlays the session's opencode process environment.
	Env map[string]string `json:"env,omitempty"`
	// ExtraPathDirs are absolute directories prepended, in order, to the PATH
	// of this session's opencode process.
	ExtraPathDirs []string `json:"extraPathDirs,omitempty"`
	// OutputSchema requests native structured output.
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Effort is a reasoning-level value passed unchanged to opencode.
	Effort string `json:"effort,omitempty"`
}

// OpenCodeOption configures OpenCodeOptions values.
type OpenCodeOption func(*OpenCodeOptions)

// NewOpenCodeOptions constructs OpenCodeOptions from functional options.
func NewOpenCodeOptions(opts ...OpenCodeOption) OpenCodeOptions {
	options := OpenCodeOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return options.clone()
}

// WithOpenCodeModel configures the session model as "provider/id".
func WithOpenCodeModel(model string) OpenCodeOption {
	return func(options *OpenCodeOptions) { options.Model = model }
}

// WithOpenCodeEnv configures the session environment overlay.
func WithOpenCodeEnv(env map[string]string) OpenCodeOption {
	cloned := cloneStringMap(env)

	return func(options *OpenCodeOptions) { options.Env = cloneStringMap(cloned) }
}

// WithOpenCodeExtraPathDirs configures the directories prepended to the session PATH.
func WithOpenCodeExtraPathDirs(dirs ...string) OpenCodeOption {
	cloned := slices.Clone(dirs)

	return func(options *OpenCodeOptions) { options.ExtraPathDirs = slices.Clone(cloned) }
}

// WithOpenCodeOutputSchema configures native structured output.
func WithOpenCodeOutputSchema(schema map[string]any) OpenCodeOption {
	cloned := cloneAnyMap(schema)

	return func(options *OpenCodeOptions) { options.OutputSchema = cloneAnyMap(cloned) }
}

// WithOpenCodeEffort configures the reasoning level passed to opencode.
func WithOpenCodeEffort(level string) OpenCodeOption {
	return func(options *OpenCodeOptions) { options.Effort = level }
}

// Meta returns exactly {"opencode": {"options": {...}}} with the selected fields.
func (options OpenCodeOptions) Meta() map[string]any {
	values := map[string]any{}
	if options.Mode != "" {
		values[metaModeKey] = options.Mode
	}

	if options.Permission != "" {
		values[metaPermissionKey] = options.Permission
	}

	if options.Model != "" {
		values[metaModelKey] = options.Model
	}

	if options.Env != nil {
		values[metaEnvKey] = cloneStringMap(options.Env)
	}

	if options.ExtraPathDirs != nil {
		values[metaExtraPathDirsKey] = slices.Clone(options.ExtraPathDirs)
	}

	if options.OutputSchema != nil {
		values[metaOutputSchemaKey] = cloneAnyMap(options.OutputSchema)
	}

	if options.Effort != "" {
		values[metaEffortKey] = options.Effort
	}

	return map[string]any{vendor: map[string]any{metaOptionsKey: values}}
}

func (options OpenCodeOptions) clone() OpenCodeOptions {
	cloned := options
	cloned.Env = cloneStringMap(options.Env)
	cloned.ExtraPathDirs = slices.Clone(options.ExtraPathDirs)
	cloned.OutputSchema = cloneAnyMap(options.OutputSchema)

	return cloned
}

// ValidateOpenCodeSessionMeta runs the owned-namespace parsing of a session
// lifecycle request's _meta without an Agent and returns the same refusal.
func ValidateOpenCodeSessionMeta(meta map[string]any) error {
	_, err := parseSessionMeta(meta)
	if err != nil {
		return err
	}

	return nil
}

// sessionMeta is what one session lifecycle request's _meta.opencode carried.
type sessionMeta struct {
	options   OpenCodeOptions
	rawEvents bool
	// present records which carrier fields the request named, so a load or
	// resume inherits the stored value only for fields it left out.
	presentEnv           bool
	presentExtraPathDirs bool
}

// parseSessionMeta validates the owned _meta.opencode namespace of one session
// lifecycle request. Unknown own-namespace keys fail closed; foreign
// namespaces are ignored; the lifecycle literal is refused by name.
func parseSessionMeta(meta map[string]any) (sessionMeta, *acp.RequestError) {
	if refusal := lifecycle.RejectKey(meta); refusal != nil {
		return sessionMeta{}, invalidParam(refusal)
	}

	raw, exists := meta[vendor]
	if !exists {
		return sessionMeta{}, nil
	}

	vendorMeta, ok := raw.(map[string]any)
	if !ok {
		return sessionMeta{}, wire.Unsupported("_meta." + vendor)
	}

	parsed := sessionMeta{}

	for key := range vendorMeta {
		switch key {
		case metaOptionsKey, metaRawEventKey:
		default:
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + key)
		}
	}

	if rawEvent, ok := vendorMeta[metaRawEventKey]; ok {
		values, ok := rawEvent.(map[string]any)
		if !ok {
			return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey)
		}

		for key, item := range values {
			enabled, ok := item.(bool)
			if key != metaEnabledKey || !ok {
				return sessionMeta{}, wire.Unsupported("_meta." + vendor + "." + metaRawEventKey + "." + key)
			}

			parsed.rawEvents = enabled
		}
	}

	rawOptions, hasOptions := vendorMeta[metaOptionsKey]
	if !hasOptions {
		return parsed, nil
	}

	values, isObject := rawOptions.(map[string]any)
	if !isObject {
		return sessionMeta{}, wire.Unsupported(metaOptionPath(""))
	}

	options, err := parseOpenCodeOptions(values)
	if err != nil {
		return sessionMeta{}, err
	}

	parsed.options = options
	_, parsed.presentEnv = values[metaEnvKey]
	_, parsed.presentExtraPathDirs = values[metaExtraPathDirsKey]

	return parsed, nil
}

func parseOpenCodeOptions(values map[string]any) (OpenCodeOptions, *acp.RequestError) {
	options := OpenCodeOptions{}

	for key, item := range values {
		switch key {
		case metaModeKey, metaPermissionKey:
			value, ok := item.(string)
			if !ok || value == "" {
				return OpenCodeOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			if key == metaModeKey {
				options.Mode = value
			} else {
				options.Permission = value
			}
		case metaModelKey:
			model, ok := item.(string)
			if !ok {
				return OpenCodeOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			options.Model = model
		case metaEnvKey:
			env, err := stringMapOption(item, metaOptionPath(key))
			if err != nil {
				return OpenCodeOptions{}, err
			}

			options.Env = env
		case metaExtraPathDirsKey:
			dirs, err := stringSliceOption(item, metaOptionPath(key))
			if err != nil {
				return OpenCodeOptions{}, err
			}

			options.ExtraPathDirs = dirs
		case metaOutputSchemaKey:
			schema, ok := item.(map[string]any)
			if !ok {
				return OpenCodeOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			options.OutputSchema = cloneAnyMap(schema)
		case metaEffortKey:
			level, ok := item.(string)
			if !ok || level == "" {
				return OpenCodeOptions{}, wire.Unsupported(metaOptionPath(key))
			}

			options.Effort = level
		default:
			return OpenCodeOptions{}, wire.Unsupported(metaOptionPath(key))
		}
	}

	return options, validateOpenCodeOptions(options)
}

func validateOpenCodeOptions(options OpenCodeOptions) *acp.RequestError {
	if options.OutputSchema != nil && len(options.OutputSchema) == 0 {
		return wire.Unsupported(metaOptionPath(metaOutputSchemaKey))
	}

	if options.Model != "" {
		if err := opencode.ModelSelectionShapeError(options.Model); err != nil {
			return wire.Unsupported(metaOptionPath(metaModelKey))
		}
	}

	if options.Permission != "" && !slices.Contains([]string{"ask", "allow", "deny"}, options.Permission) {
		return wire.Unsupported(metaOptionPath(metaPermissionKey))
	}

	if err := process.ValidateNames(options.Env); err != nil {
		var nameErr *process.NameError
		if errors.As(err, &nameErr) {
			return wire.Unsupported(metaOptionPath(metaEnvKey) + "." + nameErr.Key)
		}

		return wire.Unsupported(metaOptionPath(metaEnvKey))
	}

	if err := process.ValidateExtraPathDirs(options.ExtraPathDirs); err != nil {
		var dirErr *process.PathDirError
		if errors.As(err, &dirErr) {
			return wire.Unsupported(fmt.Sprintf("%s[%d]", metaOptionPath(metaExtraPathDirsKey), dirErr.Index))
		}

		return wire.Unsupported(metaOptionPath(metaExtraPathDirsKey))
	}

	return nil
}

func metaOptionPath(key string) string {
	path := "_meta." + vendor + "." + metaOptionsKey
	if key == "" {
		return path
	}

	return path + "." + key
}

func stringMapOption(value any, path string) (map[string]string, *acp.RequestError) {
	switch typed := value.(type) {
	case map[string]string:
		return cloneStringMap(typed), nil
	case map[string]any:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, wire.Unsupported(path + "." + key)
			}

			result[key] = text
		}

		return result, nil
	default:
		return nil, wire.Unsupported(path)
	}
}

func stringSliceOption(value any, path string) ([]string, *acp.RequestError) {
	switch typed := value.(type) {
	case []string:
		return slices.Clone(typed), nil
	case []any:
		result := make([]string, 0, len(typed))
		for index, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, wire.Unsupported(fmt.Sprintf("%s[%d]", path, index))
			}

			result = append(result, text)
		}

		return result, nil
	default:
		return nil, wire.Unsupported(path)
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

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAny(item)
		}

		return cloned
	case []string:
		return slices.Clone(typed)
	default:
		return typed
	}
}

func mergeAnyMap(base map[string]any, overlay map[string]any) map[string]any {
	result := cloneAnyMap(base)
	if result == nil {
		result = map[string]any{}
	}

	for key, value := range overlay {
		if valueMap, ok := value.(map[string]any); ok {
			if existing, ok := result[key].(map[string]any); ok {
				result[key] = mergeAnyMap(existing, valueMap)

				continue
			}
		}

		result[key] = cloneAny(value)
	}

	return result
}

// WithOpenCodeMode selects a native agent.
func WithOpenCodeMode(mode string) OpenCodeOption { return func(o *OpenCodeOptions) { o.Mode = mode } }

// WithOpenCodePermission selects native tool permission behavior.
func WithOpenCodePermission(permission string) OpenCodeOption {
	return func(o *OpenCodeOptions) { o.Permission = permission }
}
