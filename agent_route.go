package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
)

var newTurnNonceRead = rand.Read

func NewTurnNonce() (string, error) {
	var value [32]byte
	if _, err := newTurnNonceRead(value[:]); err != nil {
		return "", fmt.Errorf("create turn nonce: %w", err)
	}

	return hex.EncodeToString(value[:]), nil
}

const (
	routeEnvelopeKey       = "acp-go.dev/route"
	routeEnvelopeVersion   = 1
	metaFieldVersion       = "version"
	routeTurnNonceMaxBytes = 4 * 1024
	routeFieldVersion      = "version"
	routeFieldTurnNonce    = "turnNonce"
	routeFieldRequestID    = "requestId"
)

type inboundTurnRoute struct {
	Version   int
	TurnNonce string
}

type turnRouteContextKey struct{}

func routeCarrier(turnNonce string) map[string]any {
	return map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: routeEnvelopeVersion, routeFieldTurnNonce: turnNonce}}
}

func requestRouteCarrier(turnNonce string) map[string]any {
	if !validRouteTurnNonce(turnNonce) {
		return nil
	}

	return routeCarrier(turnNonce)
}

func validRouteTurnNonce(turnNonce string) bool {
	return strings.TrimSpace(turnNonce) != "" && len(turnNonce) <= routeTurnNonceMaxBytes
}

func withTurnRoute(ctx context.Context, turnNonce string) context.Context {
	return context.WithValue(ctx, turnRouteContextKey{}, turnNonce)
}

func turnNonceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	turnNonce, _ := ctx.Value(turnRouteContextKey{}).(string)

	return turnNonce
}

func turnRouteMetaFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}

	turnNonce := turnNonceFromContext(ctx)
	if turnNonce == "" {
		return nil
	}

	return routeCarrier(turnNonce)
}

// routeMetaPath is the request path a route refusal names. It is the reserved
// family literal spelled as a JSON path, so a host reads one field value and
// knows exactly which key it got wrong.
const routeMetaPath = `_meta["` + routeEnvelopeKey + `"]`

func routeMemberPath(member string) string {
	return routeMetaPath + "." + member
}

// parseInboundTurnRoute reads the reserved turn route envelope. The refusal is
// the uniform -32602 {error, field} data: an absent key is `missing` on the bare
// path, and a present but unacceptable value is `unsupported` naming the
// offending member — `version`, `turnNonce`, or the unknown key — falling back
// to the bare path when the value as a whole is not an object. It never carries
// a prose token or a `reason` member.
func parseInboundTurnRoute(meta map[string]any) (inboundTurnRoute, error) {
	raw, ok := meta[routeEnvelopeKey]
	if !ok {
		return inboundTurnRoute{}, missingField(routeMetaPath)
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		return inboundTurnRoute{}, unsupportedField(routeMetaPath)
	}

	// Unknown members are named before the known ones so a host that added a
	// field learns about the field rather than about a value it got right. The
	// key set is sorted so two unknown members always produce the same verdict.
	unknown := make([]string, 0, len(obj))

	for key := range obj {
		if key != routeFieldVersion && key != routeFieldTurnNonce {
			unknown = append(unknown, key)
		}
	}

	if len(unknown) > 0 {
		slices.Sort(unknown)

		return inboundTurnRoute{}, unsupportedField(routeMemberPath(unknown[0]))
	}

	version, ok := routeIntegerValue(obj[routeFieldVersion])
	if !ok || version != routeEnvelopeVersion {
		return inboundTurnRoute{}, unsupportedField(routeMemberPath(routeFieldVersion))
	}

	nonce, ok := obj[routeFieldTurnNonce].(string)
	if !ok || !validRouteTurnNonce(nonce) {
		return inboundTurnRoute{}, unsupportedField(routeMemberPath(routeFieldTurnNonce))
	}

	return inboundTurnRoute{Version: routeEnvelopeVersion, TurnNonce: nonce}, nil
}

// routeIntegerValue reads one JSON integer version. A decoded wire value arrives
// as a float64 and an embedding Go host writes an int; both name the same
// integer, and a fractional value names none.
func routeIntegerValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		if value != math.Trunc(value) || value < math.MinInt32 || value > math.MaxInt32 {
			return 0, false
		}

		return int(value), true
	case int:
		return value, true
	case json.Number:
		wide, ok := handoffInteger(json.RawMessage(value))

		return int(wide), ok && wide >= math.MinInt32 && wide <= math.MaxInt32
	default:
		return 0, false
	}
}

// invalidRoute refuses a route this adapter itself resolved against live turn
// state — a stale cancel, or a native callback naming a tool call the session
// never published. Neither reaches a host as a response: a cancel is a
// notification and answers wire-silently, and a native refusal goes back to the
// harness. The wire-facing envelope refusals are the uniform shapes above.
func invalidRoute(reason string) error {
	return acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid_route_envelope", "reason": reason})
}

func outboundRoute(scope elicitationScope) (map[string]any, error) {
	if scope.SessionID == "" || strings.TrimSpace(scope.TurnNonce) == "" {
		return nil, fmt.Errorf("turn-scoped elicitation route is incomplete")
	}

	if len(scope.TurnNonce) > routeTurnNonceMaxBytes {
		return nil, fmt.Errorf("turn-scoped elicitation route turnNonce exceeds the maximum size")
	}

	envelope := map[string]any{routeFieldVersion: routeEnvelopeVersion, jsonFieldSessionID: scope.SessionID, routeFieldTurnNonce: scope.TurnNonce}
	switch {
	case scope.ToolCallID != "" && scope.RequestID == nil:
		envelope["toolCallId"] = scope.ToolCallID
	case scope.ToolCallID == "" && scope.RequestID != nil &&
		scope.RequestID.Str != nil && *scope.RequestID.Str != "" && scope.RequestID.Number == nil:
		envelope[routeFieldRequestID] = string(*scope.RequestID.Str)
	default:
		return nil, fmt.Errorf("turn-scoped elicitation route must contain exactly one correlation")
	}

	return envelope, nil
}
