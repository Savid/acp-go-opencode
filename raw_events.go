package opencodeacp

import (
	"encoding/json"
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
	outputSchemaOptionPath  = "_meta.opencode.options.outputSchema"
	structuredOutputPath    = "_meta.opencode.structuredOutput"
	configModel             = "model"
	configMode              = "mode"
	configTypeSelect        = "select"
	modelFieldSessionMeta   = "_meta.opencode.options.model"
	modelFieldPrompt        = "model"
	openCodePermissionAsk   = "ask"
	openCodePermissionAllow = "allow"
	openCodePermissionDeny  = "deny"
	jsonFieldError          = "error"
	jsonFieldMessage        = "message"
	jsonFieldMethod         = "method"
	jsonFieldRequest        = "request"
	jsonFieldSessionID      = "sessionId"
	jsonFieldCwd            = "cwd"
	jsonFieldSequence       = "sequence"
	jsonFieldEvent          = "event"
	jsonFieldSource         = "source"
	jsonFieldCause          = "cause"
	jsonFieldStatusCode     = "statusCode"
	jsonFieldProviderCode   = "providerCode"

	jsonFieldStructuredOutputRequested = "structuredOutputRequested"
	jsonFieldField                     = "field"
	jsonFieldServer                    = "server"
	jsonFieldCommand                   = "command"
	jsonFieldMessageID                 = "messageId"
	errValueAgentClosed                = "agent closed"
	jsonFieldMode                      = "mode"
	jsonFieldURL                       = "url"
	jsonFieldMime                      = "mime"
	jsonFieldTool                      = "tool"
	jsonFieldType                      = "type"
	jsonFieldValue                     = "value"
	jsonFieldLimit                     = "limit"
	jsonFieldTitle                     = "title"
	jsonFieldScope                     = "scope"
	jsonFieldKey                       = "key"
	jsonFieldPath                      = "path"
	validationRequired                 = "required"
	validationDuplicate                = "duplicate"
	errValueBackpressure               = "backpressure"
	errValueUnsupported                = "unsupported"
	errValueSessionUnknown             = "unknown session"
	elicitationModeForm                = "form"
	elicitationModeURL                 = "url"

	rawMarkerTruncated      = "truncated"
	rawMarkerReason         = "reason"
	rawMarkerMaxBytes       = "maxBytes"
	rawMarkerSizeBytes      = "sizeBytes"
	rawReasonOversize       = "oversize"
	rawReasonUnserializable = "unserializable"

	rawEventSource     = "opencode-serve"
	limitSessionPrompt = "session_prompt"
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

	marker := map[string]any{
		rawMarkerTruncated: true,
		rawMarkerMaxBytes:  rawEventMaxBytes,
	}
	if err != nil {
		marker[rawMarkerReason] = rawReasonUnserializable
	} else {
		marker[rawMarkerReason] = rawReasonOversize
		marker[rawMarkerSizeBytes] = len(encoded)
	}

	capped := map[string]any{
		jsonFieldSessionID: payload[jsonFieldSessionID],
		jsonFieldSequence:  payload[jsonFieldSequence],
		jsonFieldSource:    payload[jsonFieldSource],
		jsonFieldEvent:     marker,
	}
	if meta, ok := payload["_meta"]; ok {
		capped["_meta"] = meta
	}

	return capped
}
