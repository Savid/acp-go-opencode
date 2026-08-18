//nolint:tagliatelle // OpenCode native event payloads use sessionID wire names.
package opencode

import "encoding/json"

// Native event type names this package decodes structurally. They are the native
// spellings, and they stay here.
const (
	// EventPermissionAsked and EventPermissionV2Asked announce a permission
	// request awaiting an answer.
	EventPermissionAsked   = "permission.asked"
	EventPermissionV2Asked = "permission.v2.asked"
	// EventPermissionReplied and EventPermissionV2Replied confirm, from the
	// server, that a permission request was answered.
	EventPermissionReplied   = "permission.replied"
	EventPermissionV2Replied = "permission.v2.replied"
	// EventQuestionAsked and EventQuestionV2Asked announce a non-permission
	// question awaiting an answer.
	EventQuestionAsked   = "question.asked"
	EventQuestionV2Asked = "question.v2.asked"
	// EventQuestionReplied and EventQuestionV2Replied confirm, from the server,
	// that a question was answered.
	EventQuestionReplied   = "question.replied"
	EventQuestionV2Replied = "question.v2.replied"
)

// ActionRepliedEvent reports a server-confirmed resolution of one permission or
// question request. The native payload names the session and the request it
// resolved, which is the whole correlation a reader needs: the request id it
// carries is the id the matching `*.asked` announced.
type ActionRepliedEvent struct {
	SessionID string `json:"sessionID"`
	RequestID string `json:"requestID"`
	// Reply is the permission answer OpenCode recorded. It is empty on a
	// question resolution, which records answers instead.
	Reply string `json:"reply"`
}

// Accepted reports whether the recorded permission reply granted the request.
// A question resolution grants nothing by itself, so it is never accepted here.
func (e ActionRepliedEvent) Accepted() bool {
	return e.Reply == PermissionReplyOnce || e.Reply == PermissionReplyAlways
}

// Permission reply values OpenCode accepts.
const (
	PermissionReplyOnce   = "once"
	PermissionReplyAlways = "always"
	PermissionReplyReject = "reject"
)

// DecodeActionReplied reads a `permission[.v2].replied` or
// `question[.v2].replied` payload. It reports false for a payload that names no
// request, because a resolution that cannot be correlated resolves nothing.
func DecodeActionReplied(properties json.RawMessage) (ActionRepliedEvent, bool) {
	var replied ActionRepliedEvent
	if err := json.Unmarshal(properties, &replied); err != nil {
		return ActionRepliedEvent{}, false
	}

	if replied.SessionID == "" || replied.RequestID == "" {
		return ActionRepliedEvent{}, false
	}

	return replied, true
}

// EventSessionID reports the native session one event concerns. Every event this
// adapter routes carries its session either directly or inside the one entity the
// event is about, and an event that names none concerns no session: routing by a
// structured id is what keeps a directory-scoped stream from delivering one
// session's work to another, including a forked child sharing that directory.
func EventSessionID(event Event) (string, bool) {
	var direct struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(event.Properties, &direct); err == nil && direct.SessionID != "" {
		return direct.SessionID, true
	}

	var nested struct {
		Info struct {
			SessionID string `json:"sessionID"`
		} `json:"info"`
		Part struct {
			SessionID string `json:"sessionID"`
		} `json:"part"`
	}
	if err := json.Unmarshal(event.Properties, &nested); err != nil {
		return "", false
	}

	for _, candidate := range []string{nested.Info.SessionID, nested.Part.SessionID} {
		if candidate != "" {
			return candidate, true
		}
	}

	return "", false
}
