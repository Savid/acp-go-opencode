package opencodeacp

import (
	"context"
	"errors"
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

	_, err = outboundRoute(elicitationScope{})
	require.ErrorContains(t, err, "incomplete")
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce"})
	require.ErrorContains(t, err, "exactly one")

	requestID := acp.RequestId{Number: requestIDNumberPointer(7)}
	_, err = outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "exactly one")

	requestIDValue := acp.RequestIdStr("request")
	requestID = acp.RequestId{Str: &requestIDValue}
	route, err := outboundRoute(elicitationScope{SessionID: "session", TurnNonce: "nonce", RequestID: &requestID})
	require.NoError(t, err)
	require.Equal(t, "request", route["requestId"])

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
