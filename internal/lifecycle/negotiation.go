package lifecycle

import (
	"encoding/json"
	"math"
)

// MetaPath is the request path a rejection names. Negotiation and correlation
// values are rejected as invalid params rather than as stream violations,
// because they are read before any stream exists.
const MetaPath = `_meta["` + MetaKey + `"]`

// ParamError refuses a negotiation or correlation value, or names the reserved
// key on a surface that carries none. It names the exact member path so a host
// can tell which value it got wrong, and it is the one family literal this
// adapter validates on `initialize` itself.
type ParamError struct {
	// Field is the full request path, from MetaPath down to the offending
	// member.
	Field string
	// Missing distinguishes the two verdicts a host reads from this path. It is
	// true only when the contract requires the key on this surface and the
	// caller left it out; a value that is present and refused — the key on a
	// surface that carries none, a malformed member — is never missing.
	Missing bool
}

// Error implements error.
func (e *ParamError) Error() string {
	if e.Missing {
		return "missing " + e.Field
	}

	return "unsupported " + e.Field
}

func paramError(members ...string) *ParamError {
	field := MetaPath
	for _, member := range members {
		field += "." + member
	}

	return &ParamError{Field: field}
}

// missingParamError names the reserved key a surface requires and the caller
// omitted.
func missingParamError() *ParamError {
	return &ParamError{Field: MetaPath, Missing: true}
}

// RefuseKey reports the refusal a surface carrying no lifecycle value answers
// with when the reserved key is present. A family literal is never a foreign
// namespace and never a no-op, so the refusal names the exact path rather than
// the surface.
func RefuseKey(meta map[string]any) *ParamError {
	if _, present := meta[MetaKey]; !present {
		return nil
	}

	return paramError()
}

// DecodeCapability reads the capability from `InitializeRequest._meta`. An absent value is
// reported as not present rather than as a refusal: the host asked for nothing,
// and the answer, every envelope, and every correlation read are then omitted for
// the whole connection.
func DecodeCapability(meta map[string]any) (bool, *ParamError) {
	raw, present := meta[MetaKey]
	if !present {
		return false, nil
	}

	fields, ok := raw.(map[string]any)
	if !ok {
		return false, paramError()
	}

	for key := range fields {
		if key != fieldVersion {
			return false, paramError(key)
		}
	}

	version, ok := integerValue(fields[fieldVersion])
	if !ok || version != Version {
		return false, paramError(fieldVersion)
	}

	return true, nil
}

// Submission names one accepted client prompt. The client nonce is the host's own
// input identity: it is distinct from every JSON-RPC message id and from the
// route envelope's turn nonce, and none is ever substituted for another.
type Submission struct {
	SubmissionID string
	ClientNonce  string
	RunID        string
}

// DecodePromptCorrelation reads the value a `session/prompt` carries while
// version 1 is negotiated. The key is required when negotiated and forbidden when
// not, and either way the verdict is reached before the prompt is dispatched, so
// no frame is written to the harness.
func DecodePromptCorrelation(meta map[string]any, negotiated Negotiated) (Submission, *ParamError) {
	raw, present := meta[MetaKey]

	switch {
	case !negotiated.Present() && present:
		return Submission{}, paramError()
	case !negotiated.Present():
		return Submission{}, nil
	case !present:
		return Submission{}, missingParamError()
	}

	fields, ok := raw.(map[string]any)
	if !ok {
		return Submission{}, paramError()
	}

	for key := range fields {
		if key != fieldVersion && key != fieldSubmission {
			return Submission{}, paramError(key)
		}
	}

	if refusal := checkCorrelationVersion(fields, negotiated); refusal != nil {
		return Submission{}, refusal
	}

	return decodeSubmission(fields[fieldSubmission])
}

func checkCorrelationVersion(fields map[string]any, negotiated Negotiated) *ParamError {
	version, ok := integerValue(fields[fieldVersion])
	if !ok || version != Version || !negotiated.Present() {
		return paramError(fieldVersion)
	}

	return nil
}

// integerValue reads one JSON integer. A decoded wire value arrives as a float64
// and an embedding Go host writes an int, so both are the same integer; a
// fractional value is neither.
//
// Integrality alone is not enough on the float64 branch. A magnitude beyond the
// integers an int can hold — 1e300 is one — has no fractional part and still names
// no int at all, and converting it would be undefined rather than wrong in a
// stated way. Such a value is refused as the unsupported member it is, so the only
// float64 that reads as an integer is one the target holds exactly.
func integerValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		// 2^63 is the first float64 magnitude above every int64, and -2^63 is
		// exactly the least, so a value inside these bounds converts exactly.
		if value != math.Trunc(value) || value < math.MinInt64 || value >= 1<<63 {
			return 0, false
		}

		wide := int64(value)

		return int(wide), int64(int(wide)) == wide
	case int:
		return value, true
	case json.Number:
		number, err := value.Int64()

		return int(number), err == nil
	default:
		return 0, false
	}
}

func decodeSubmission(raw any) (Submission, *ParamError) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return Submission{}, paramError(fieldSubmission)
	}

	for key := range fields {
		if key != fieldSubmissionID && key != fieldClientNonce && key != fieldRunID {
			return Submission{}, paramError(fieldSubmission, key)
		}
	}

	submission := Submission{}

	for _, member := range []struct {
		key      string
		target   *string
		required bool
	}{
		{fieldSubmissionID, &submission.SubmissionID, true},
		{fieldClientNonce, &submission.ClientNonce, true},
		{fieldRunID, &submission.RunID, false},
	} {
		value, refusal := correlationIdentifier(fields, member.key, member.required)
		if refusal != nil {
			return Submission{}, refusal
		}

		*member.target = value
	}

	return submission, nil
}

// correlationIdentifier reads one opaque handle. An identifier is a correlation
// handle, not a payload: it is bounded, and it is never empty — an optional one
// is omitted rather than emptied, so a member present carrying the empty string
// is malformed rather than absent.
func correlationIdentifier(fields map[string]any, key string, required bool) (string, *ParamError) {
	raw, present := fields[key]
	if !present {
		if required {
			return "", paramError(fieldSubmission, key)
		}

		return "", nil
	}

	value, ok := raw.(string)
	if !ok || value == "" || len(value) > IdentifierBound {
		return "", paramError(fieldSubmission, key)
	}

	return value, nil
}

// ActionCorrelation is the value this adapter stamps on every
// `session/request_permission` and every `elicitation/create` while version 1 is
// negotiated. It names the emitting stream and the registered action, and it is
// lifecycle identity only: the route object remains the routing and
// authentication envelope wherever one applies.
type ActionCorrelation struct {
	StreamID string
	ActionID string
	Owner    Owner
	RunID    string
}

// Value renders the correlation for a request's `_meta`. It carries exactly the
// four fixed members the contract fixes, with `runId` present only when the
// action names one.
func (c ActionCorrelation) Value() map[string]any {
	action := map[string]any{
		fieldActionID: c.ActionID,
		fieldOwner: map[string]any{
			fieldType: string(c.Owner.Type),
			fieldID:   c.Owner.ID,
		},
	}
	withOptional(action, fieldRunID, c.RunID)

	return map[string]any{
		fieldVersion:  Version,
		fieldStreamID: c.StreamID,
		fieldAction:   action,
	}
}
