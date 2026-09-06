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
	outputSchemaOptionPath  = "_meta.opencode.options.outputSchema"
	structuredOutputPath    = "_meta.opencode.structuredOutput"
	configModel             = "model"
	configMode              = "mode"
	// defaultMode is the native agent OpenCode runs when no other is named. It
	// is a starting value only: what a session addresses its frames with is
	// whatever it was last told, judged by OpenCode rather than by this adapter.
	defaultMode             = "build"
	configTypeSelect        = "select"
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
	jsonFieldAction         = "action"
	jsonFieldResources      = "resources"
	jsonFieldCause          = "cause"
	jsonFieldStatusCode     = "statusCode"
	jsonFieldProviderCode   = "providerCode"

	jsonFieldStructuredOutputRequested = "structuredOutputRequested"
	jsonFieldField                     = "field"
	jsonFieldServer                    = "server"
	jsonFieldCommand                   = "command"
	jsonFieldMessageID                 = "messageId"
	valAgentClosed                     = "agent closed"
	// The closed off-prompt `-32603` vocabulary. Every internal error this
	// adapter answers with off the prompt-turn path carries exactly one of these
	// tokens in `data.error`, `message` stays the JSON-RPC constant, and the data
	// never carries native or Go text. valInvalidOptions is the
	// construction verdict, valRestoreFailed names a stored session this
	// adapter will not replay, valRuntimeUnavailable names a shared runtime
	// that is gone and un-containable, valSessionPoisoned names a session
	// that refuses everything but close and delete, and valInternalFailure
	// is the catch-all.
	valInvalidOptions     = "opencode_invalid_options"
	valRestoreFailed      = "opencode_restore_failed"
	valRuntimeUnavailable = "opencode_runtime_unavailable"
	valSessionPoisoned    = "opencode_session_poisoned"
	valInternalFailure    = "opencode_internal_failure"
	jsonFieldClass        = "class"
	jsonFieldParams       = "params"
	// The closed `class` vocabulary valInternalFailure may carry.
	// classNativeStartup names a loopback call that failed while a runtime or a
	// native session was being started; classSessionReplacementRaced names a
	// logical session whose active incarnation changed under a replacement.
	classNativeStartup           = "native_startup"
	classSessionReplacementRaced = "session_replacement_raced"
	jsonFieldMode                = "mode"
	jsonFieldURL                 = "url"
	jsonFieldMime                = "mime"
	jsonFieldTool                = "tool"
	jsonFieldType                = "type"
	jsonFieldSchema              = "schema"
	jsonFieldEnum                = "enum"
	jsonFieldData                = "data"
	jsonFieldEvents              = "events"
	jsonFieldFormat              = "format"
	jsonFieldDescription         = "description"
	jsonFieldItems               = "items"
	jsonFieldTime                = "time"
	jsonFieldValue               = "value"
	jsonFieldLimit               = "limit"
	jsonFieldTitle               = "title"
	jsonFieldScope               = "scope"
	jsonFieldKey                 = "key"
	jsonFieldPath                = "path"
	jsonFieldIndex               = "index"
	jsonFieldStage               = "stage"
	jsonFieldReason              = "reason"
	jsonFieldSizeBytes           = "sizeBytes"
	jsonFieldMaxBytes            = "maxBytes"
	jsonTypeArray                = "array"
	validationRequired           = "required"
	validationDuplicate          = "duplicate"
	valBackpressure              = "backpressure"
	valUnsupported               = "unsupported"
	// valMissing is the -32602 verdict for a reserved key the contract
	// requires and the caller left out. It is distinct from valUnsupported,
	// which names a value that is present and refused, and the two are never
	// collapsed: a host reading `missing` fixes its own request, a host reading
	// `unsupported` on a bare key path stops sending the key on that surface.
	valMissing             = "missing"
	valNoTransport         = "no_transport"
	valSessionUnknown      = "unknown session"
	valSharedRuntimeExited = "shared OpenCode runtime exited"
	elicitationModeForm    = "form"
	elicitationModeURL     = "url"

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

func capRawEventPayload(payload map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(payload)
	if err == nil && len(encoded) <= rawEventMaxBytes {
		return payload, nil
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

	final, finalErr := json.Marshal(capped)
	if finalErr != nil {
		return nil, fmt.Errorf("marshal capped raw event payload: %w", finalErr)
	}

	if len(final) > rawEventMaxBytes {
		return nil, fmt.Errorf("capped raw event payload is %d bytes, exceeds %d", len(final), rawEventMaxBytes)
	}

	return capped, nil
}
