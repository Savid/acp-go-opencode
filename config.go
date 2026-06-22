package opencodeacp

import (
	"errors"
	"fmt"
	"maps"
	"strings"
)

const (
	// ConfigOpenCodePathKey is the config-map key for the OpenCode executable.
	ConfigOpenCodePathKey = "opencode-path"
	// ConfigEnvKey is the config-map key for process environment overrides.
	ConfigEnvKey = "env"
)

// Config is the stable map-friendly configuration subset for embedding hosts.
type Config struct {
	// OpenCodePath is the OpenCode executable path. If empty, PATH is searched.
	OpenCodePath string
	// Env is merged into the launched OpenCode process environment.
	Env map[string]string
}

// ParseConfig parses a small map-based config used by embedding hosts.
func ParseConfig(values map[string]any) (Config, error) {
	out := Config{Env: map[string]string{}}
	if values == nil {
		return out, nil
	}

	for key, value := range values {
		switch key {
		case ConfigOpenCodePathKey:
			if value == nil {
				continue
			}
			path, ok := value.(string)
			if !ok {
				return Config{}, fmt.Errorf("%s must be a string", ConfigOpenCodePathKey)
			}
			out.OpenCodePath = strings.TrimSpace(path)
		case ConfigEnvKey:
			env, err := envMapValue(value)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", ConfigEnvKey, err)
			}
			out.Env = env
		default:
			return Config{}, fmt.Errorf("unsupported opencode config key %q", key)
		}
	}

	return out, nil
}

// ValidateConfig validates a map-based config without returning parsed values.
func ValidateConfig(values map[string]any) error {
	_, err := ParseConfig(values)

	return err
}

// Options returns Serve options for the parsed config.
func (config Config) Options() []Option {
	opts := make([]Option, 0, 2)
	if config.OpenCodePath != "" {
		opts = append(opts, WithOpenCodePath(config.OpenCodePath))
	}
	if config.Env != nil {
		opts = append(opts, WithEnv(config.Env))
	}

	return opts
}

func envMapValue(raw any) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}

	switch typed := raw.(type) {
	case map[string]string:
		out := make(map[string]string, len(typed))
		maps.Copy(out, typed)

		return out, nil
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, value := range typed {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string", key)
			}
			out[key] = text
		}

		return out, nil
	default:
		return nil, errors.New("must be a string map")
	}
}
