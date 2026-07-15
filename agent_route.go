package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

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
	routeEnvelopeKey     = "acp-go.dev/route"
	routeEnvelopeVersion = 1
	routeFieldVersion    = "version"
	routeFieldTurnNonce  = "turnNonce"
	routeFieldRequestID  = "requestId"
)

type inboundTurnRoute struct {
	Version   int
	TurnNonce string
}

type turnRouteContextKey struct{}

func routeCarrier(turnNonce string) map[string]any {
	return map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: routeEnvelopeVersion, routeFieldTurnNonce: turnNonce}}
}

func withTurnRoute(ctx context.Context, turnNonce string) context.Context {
	return context.WithValue(ctx, turnRouteContextKey{}, turnNonce)
}

func turnRouteMetaFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}

	turnNonce, _ := ctx.Value(turnRouteContextKey{}).(string)
	if turnNonce == "" {
		return nil
	}

	return routeCarrier(turnNonce)
}

func parseInboundTurnRoute(meta map[string]any) (inboundTurnRoute, error) {
	raw, ok := meta[routeEnvelopeKey]
	if !ok {
		return inboundTurnRoute{}, invalidRoute("missing route envelope")
	}

	obj, ok := raw.(map[string]any)
	if !ok || len(obj) != 2 {
		return inboundTurnRoute{}, invalidRoute("route envelope must contain exactly version and turnNonce")
	}

	version, ok := obj[routeFieldVersion].(float64)
	if !ok {
		if value, intOK := obj[routeFieldVersion].(int); intOK {
			version, ok = float64(value), true
		}
	}

	nonce, nonceOK := obj[routeFieldTurnNonce].(string)
	if !ok || version != routeEnvelopeVersion || !nonceOK || nonce == "" {
		return inboundTurnRoute{}, invalidRoute("unknown route version or empty turnNonce")
	}

	return inboundTurnRoute{Version: routeEnvelopeVersion, TurnNonce: nonce}, nil
}

func invalidRoute(reason string) error {
	return acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid_route_envelope", "reason": reason})
}

func outboundRoute(scope elicitationScope) (map[string]any, error) {
	if scope.SessionID == "" || scope.TurnNonce == "" {
		return nil, fmt.Errorf("turn-scoped elicitation route is incomplete")
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
