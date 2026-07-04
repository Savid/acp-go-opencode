package opencodeacp

import (
	"encoding/json"
	"fmt"
)

const (
	ForkSessionMethod = "_opencode/session/fork"
	RawEventMethod    = "_opencode/rawEvent"

	opencodeMetaKey         = "opencode"
	rawEventKey             = "rawEvent"
	rawEventEnabledKey      = "enabled"
	rawEventCapabilityKey   = "rawEvent"
	rawEventEnabledByPath   = "_meta.opencode.rawEvent.enabled"
	rawEventMaxBytes        = 64 * 1024
	opencodeNativeIDMetaKey = "nativeSessionId"
	structuredOutputMetaKey = "structuredOutput"
	configModel             = "model"
	configMode              = "mode"
	configTypeSelect        = "select"
	idmapSubpath            = "idmap"
	xdgDataSubpath          = "xdg/data"
	xdgConfigSubpath        = "xdg/config"
	xdgCacheSubpath         = "xdg/cache"
	xdgStateSubpath         = "xdg/state"
	jsonFieldError          = "error"
	jsonFieldMessage        = "message"
	jsonFieldMethod         = "method"
	jsonFieldSessionID      = "sessionId"
	jsonFieldCwd            = "cwd"
	validationRequired      = "required"
)

type rawMessageConfig struct {
	enabled bool
}

func rawMessageConfigFromMeta(meta map[string]any) rawMessageConfig {
	opencodeMeta, _ := meta[opencodeMetaKey].(map[string]any)
	if opencodeMeta == nil {
		return rawMessageConfig{}
	}
	rawEvent, _ := opencodeMeta[rawEventKey].(map[string]any)
	enabled, _ := rawEvent[rawEventEnabledKey].(bool)
	if enabled {
		return rawMessageConfig{enabled: true}
	}

	return rawMessageConfig{}
}

func (c rawMessageConfig) Enabled() bool {
	return c.enabled
}

func capRawEventPayload(payload map[string]any) map[string]any {
	encoded, err := json.Marshal(payload)
	if err == nil && len(encoded) <= rawEventMaxBytes {
		return payload
	}

	return map[string]any{
		"sessionId": payload["sessionId"],
		"sequence":  payload["sequence"],
		"source":    payload["source"],
		"event": map[string]any{
			"truncated": true,
			"error":     fmt.Sprintf("raw event exceeded %d bytes", rawEventMaxBytes),
		},
	}
}
