package opencodeacp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

type failingRouteReader struct{}

func (failingRouteReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewTurnNonceSuccessAndEntropyFailure(t *testing.T) {
	nonce, err := NewTurnNonce()
	require.NoError(t, err)
	require.Len(t, nonce, 64)

	old := newTurnNonceRead
	newTurnNonceRead = failingRouteReader{}.Read
	t.Cleanup(func() { newTurnNonceRead = old })

	_, err = NewTurnNonce()
	require.ErrorContains(t, err, "create turn nonce")
}

func TestRouteEnvelopeRemainingShapes(t *testing.T) {
	_, err := parseInboundTurnRoute(map[string]any{routeEnvelopeKey: map[string]any{
		routeFieldVersion: routeEnvelopeVersion,
		"turnNonce":       "nonce",
		"extra":           true,
	}})
	require.Error(t, err)

	parsed, err := parseInboundTurnRoute(map[string]any{routeEnvelopeKey: map[string]any{
		routeFieldVersion: routeEnvelopeVersion,
		"turnNonce":       "nonce",
	}})
	require.NoError(t, err)
	require.Equal(t, inboundTurnRoute{Version: routeEnvelopeVersion, TurnNonce: "nonce"}, parsed)
	paddedNonce := "  opaque nonce  "
	parsed, err = parseInboundTurnRoute(routeCarrier(paddedNonce))
	require.NoError(t, err)
	require.Equal(t, paddedNonce, parsed.TurnNonce)
	boundaryNonce := strings.Repeat("n", routeTurnNonceMaxBytes)
	parsed, err = parseInboundTurnRoute(routeCarrier(boundaryNonce))
	require.NoError(t, err)
	require.Equal(t, boundaryNonce, parsed.TurnNonce)
	require.NotNil(t, requestRouteCarrier(boundaryNonce))
	require.Nil(t, requestRouteCarrier(""))
	require.Nil(t, requestRouteCarrier(" \t\n"))
	require.Nil(t, requestRouteCarrier(strings.Repeat("n", routeTurnNonceMaxBytes+1)))
	_, err = parseInboundTurnRoute(routeCarrier(" \t\n"))
	requireInvalidParamsData(t, err,
		map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: routeMemberPath(routeFieldTurnNonce)})
	_, err = parseInboundTurnRoute(routeCarrier(strings.Repeat("n", routeTurnNonceMaxBytes+1)))
	requireInvalidParamsData(t, err,
		map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: routeMemberPath(routeFieldTurnNonce)})

	_, err = outboundRoute(elicitationScope{})
	require.ErrorContains(t, err, "incomplete")
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: " \t\n", ToolCallID: "tool"})
	require.ErrorContains(t, err, "incomplete")
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce"})
	require.ErrorContains(t, err, "exactly one")
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: strings.Repeat("n", routeTurnNonceMaxBytes+1)})
	require.ErrorContains(t, err, "maximum size")

	requestID := acp.RequestId{Number: requestIDNumberPointer(7)}
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")

	requestIDValue := acp.RequestIdStr("request")
	requestID = acp.RequestId{Str: &requestIDValue}
	route, err := outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.NoError(t, err)
	require.Equal(t, "request", route["requestId"])
	route, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: paddedNonce, RequestID: &requestID})
	require.NoError(t, err)
	require.Equal(t, paddedNonce, route[routeFieldTurnNonce])

	emptyRequestID := acp.RequestIdStr("")
	requestID = acp.RequestId{Str: &emptyRequestID}
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")

	requestID = acp.RequestId{}
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")

	ctx := withTurnRoute(context.Background(), "turn-old")
	require.Equal(t, routeCarrier("turn-old"), turnRouteMetaFromContext(ctx))
	require.Nil(t, turnRouteMetaFromContext(nil)) //nolint:staticcheck // Explicitly verify the nil-context guard.
	require.Nil(t, turnRouteMetaFromContext(context.Background()))
}

func requestIDNumberPointer(value acp.RequestIdNumber) *acp.RequestIdNumber { return &value }

// TestRouteVersionAcceptsAnEmbeddedHostInteger proves the version reads the same
// whether it arrived as decoded JSON (float64) or was written by an embedding Go
// host (int).
func TestRouteVersionAcceptsAnEmbeddedHostInteger(t *testing.T) {
	parsed, err := parseInboundTurnRoute(map[string]any{routeEnvelopeKey: map[string]any{
		routeFieldVersion:   routeEnvelopeVersion,
		routeFieldTurnNonce: "nonce",
	}})
	require.NoError(t, err)
	require.Equal(t, routeEnvelopeVersion, parsed.Version)

	_, err = parseInboundTurnRoute(map[string]any{routeEnvelopeKey: map[string]any{
		routeFieldVersion:   "1",
		routeFieldTurnNonce: "nonce",
	}})
	requireInvalidParamsData(t, err,
		map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: routeMemberPath(routeFieldVersion)})
}
