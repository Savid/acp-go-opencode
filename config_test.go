package opencodeacp

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	t.Parallel()

	empty, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(nil) returned error: %v", err)
	}
	if empty.Env == nil || len(empty.Env) != 0 {
		t.Fatalf("nil config env = %#v", empty.Env)
	}

	cfg, err := ParseConfig(map[string]any{
		ConfigOpenCodePathKey: " /usr/bin/opencode ",
		ConfigEnvKey: map[string]any{
			"PATH": "/bin",
			"A":    "1",
		},
	})
	if err != nil {
		t.Fatalf("ParseConfig returned error: %v", err)
	}
	if cfg.OpenCodePath != "/usr/bin/opencode" {
		t.Fatalf("OpenCodePath = %q", cfg.OpenCodePath)
	}
	if !reflect.DeepEqual(cfg.Env, map[string]string{"PATH": "/bin", "A": "1"}) {
		t.Fatalf("Env = %#v", cfg.Env)
	}

	opts := cfg.Options()
	applied := applyOptions(opts)
	if applied.OpenCodePath != cfg.OpenCodePath || !reflect.DeepEqual(applied.Env, cfg.Env) {
		t.Fatalf("applied options = %#v", applied)
	}
	cfg.Env["A"] = "mutated"
	if applied.Env["A"] != "1" {
		t.Fatalf("options env was not cloned: %#v", applied.Env)
	}

	cfg, err = ParseConfig(map[string]any{
		ConfigOpenCodePathKey: nil,
		ConfigEnvKey:          map[string]string{"A": "1"},
	})
	if err != nil {
		t.Fatalf("ParseConfig map[string]string returned error: %v", err)
	}
	if cfg.OpenCodePath != "" || !reflect.DeepEqual(cfg.Env, map[string]string{"A": "1"}) {
		t.Fatalf("cfg = %#v", cfg)
	}

	cfg, err = ParseConfig(map[string]any{ConfigEnvKey: nil})
	if err != nil {
		t.Fatalf("ParseConfig nil env returned error: %v", err)
	}
	if cfg.Env == nil || len(cfg.Env) != 0 {
		t.Fatalf("nil env parsed as %#v", cfg.Env)
	}
}

func TestParseConfigErrors(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]map[string]any{
		"path type": {ConfigOpenCodePathKey: 1},
		"env type":  {ConfigEnvKey: []string{"bad"}},
		"env value": {ConfigEnvKey: map[string]any{"A": 1}},
		"unknown":   {"wat": true},
	} {
		if _, err := ParseConfig(cfg); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	if err := ValidateConfig(map[string]any{"wat": true}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ValidateConfig error = %v", err)
	}
}
