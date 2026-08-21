package opencodeacp

import (
	"context"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestCorrectionDefensiveRegistrationBranches(t *testing.T) {
	(*Agent)(nil).interruptConnection()
	(*connectionTransport)(nil).interrupt()

	registrations := newHostRequestRegistrations()
	incomplete := <-registrations.expect(hostRequestKey{})
	require.ErrorContains(t, incomplete, "incomplete lifecycle correlation")

	key := hostRequestKey{method: acp.ClientMethodSessionRequestPermission, streamID: "stream", actionID: "action"}
	first := registrations.expect(key)
	require.ErrorContains(t, <-registrations.expect(key), "already registered")
	registrations.failIfPending(key, nil)
	require.Error(t, <-first)
	(*hostRequestRegistrations)(nil).complete(key, nil)

	for _, frame := range [][]byte{
		[]byte(`{`),
		[]byte(`{"method":"other","params":{}}`),
		[]byte(`{"method":"session/request_permission","params":[]}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":[]}}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":{"acp.lifecycle.v1":[]}}}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":{"acp.lifecycle.v1":{"streamId":"","action":{"actionId":""}}}}}`),
	} {
		_, ok := hostRequestKeyFromFrame(frame)
		require.False(t, ok)
	}

	panicking := &localAgentConnection{registrations: newHostRequestRegistrations()}
	permission := panicking.BeginRequestPermission(context.Background(), acp.RequestPermissionRequest{}, key)
	require.ErrorContains(t, (<-permission.answered).err, "panicked")
	require.Error(t, <-permission.registered)

	elicitationKey := hostRequestKey{method: acp.ClientMethodElicitationCreate, streamID: "stream", actionID: "elicit"}
	elicitation := panicking.BeginCreateElicitation(
		context.Background(), acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
			Message: "choose", RequestedSchema: acp.UnstableElicitationSchema{},
		}}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, elicitationKey,
	)
	require.ErrorContains(t, (<-elicitation.answered).err, "panicked")
	require.Error(t, <-elicitation.registered)
}
