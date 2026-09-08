package opencodeacp

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// authAdmissionSettleWait is how long a test lets the leg an admission gate
// denies try to get past it. Without the gate the leg runs to completion inside
// this window; with it the leg is parked and the wait simply expires, which is
// the difference each of these tests pins.
const authAdmissionSettleWait = 500 * time.Millisecond

// authSecretFixture publishes a single api-key method for "xai", so a callback
// reaches the durable write through applySecret and the fake runtime's
// SetProviderAuth hook is the controlled point both legs can be held at.
func authSecretFixture(t *testing.T) *authFixture {
	t.Helper()

	fixture := newAuthFixture(t)
	fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {{Type: authMethodTypeAPI, Label: "Manually enter API Key"}},
	}
	fixture.refreshCatalog(t)

	return fixture
}

// TestDisconnectIsNeverUndoneByAnAdmittedInstall holds a login's durable write
// open inside the native store and runs a disconnect for the same provider
// underneath it. Both legs rewrite one credential slot, so nothing but
// serialization on that slot decides the outcome: unserialized, disconnect
// removes the slot, verifies it empty and answers success, and the install then
// refills the slot it just verified empty. The ledger correctly refuses the
// stale confirmation, which is what makes the result worse than a lost write —
// the entry reads removed, inventory skips removed, and the credential is live
// and invisible on every host surface.
func TestDisconnectIsNeverUndoneByAnAdmittedInstall(t *testing.T) {
	fixture := authSecretFixture(t)
	flow := fixture.authorize(t, nil)

	arrived := make(chan struct{})
	release := make(chan struct{})

	fixture.runtime.setAuthFunc = func(string, opencode.ProviderAuthCredential) error {
		close(arrived)
		<-release

		return nil
	}

	installed := make(chan error, 1)

	go func() {
		_, err := fixture.callback(t, flow.FlowID, "0", "sk-secret")
		installed <- err
	}()

	<-arrived

	disconnected := make(chan error, 1)

	go func() {
		_, err := fixture.disconnect(t, "conn-1", 1)
		disconnected <- err
	}()

	time.Sleep(authAdmissionSettleWait)
	close(release)

	require.NoError(t, <-disconnected)
	<-installed

	record, ok, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerRemoved, record.State)

	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()

	require.NotContains(t, fixture.runtime.storedAuth, "xai")
}

// TestOnlyOneCallbackIsAdmittedForOneFlow submits two different secrets against
// one pending flow while the first is held inside its native write. The flow is
// the unit of authorization the operator granted once, so only one leg may
// drive it: unserialized, both apply their own value, both answer saved, and
// the store keeps whichever landed last.
func TestOnlyOneCallbackIsAdmittedForOneFlow(t *testing.T) {
	fixture := authSecretFixture(t)
	flow := fixture.authorize(t, nil)

	arrived := make(chan struct{})
	release := make(chan struct{})

	var held atomic.Bool

	fixture.runtime.setAuthFunc = func(string, opencode.ProviderAuthCredential) error {
		if held.CompareAndSwap(false, true) {
			close(arrived)
			<-release
		}

		return nil
	}

	first := make(chan error, 1)

	go func() {
		_, err := fixture.callback(t, flow.FlowID, "0", "sk-first")
		first <- err
	}()

	<-arrived

	_, secondErr := fixture.callback(t, flow.FlowID, "0", "sk-second")

	close(release)

	require.NoError(t, <-first)
	requireAuthFailure(t, secondErr, authCauseFlowState)

	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()

	require.Len(t, fixture.runtime.setAuthCalls, 1)
	require.Equal(t, "sk-first", fixture.runtime.storedAuth["xai"].Key)
}

// TestConcurrentIdenticalAuthorizesMintOneFlow runs two authorize requests
// carrying the same idempotency key, with the first held between its replay
// check and the publication of its record. Unserialized the two miss each other
// entirely, both mint at the provider, and the loser is then cancelled by the
// winner's supersede — after the operator has already been shown its code.
func TestConcurrentIdenticalAuthorizesMintOneFlow(t *testing.T) {
	fixture := newAuthFixture(t)
	params := fixture.authorizeParams(t, nil)

	release := holdFirstAuthToken(t)

	type answer struct {
		result any
		err    error
	}

	firstAnswer := make(chan answer, 1)

	go func() {
		result, err := fixture.broker.authorize(context.Background(), params)
		firstAnswer <- answer{result: result, err: err}
	}()

	<-release.arrived

	secondAnswer := make(chan answer, 1)

	go func() {
		result, err := fixture.broker.authorize(context.Background(), params)
		secondAnswer <- answer{result: result, err: err}
	}()

	select {
	case <-secondAnswer:
		t.Fatal("the repeated request minted its own flow instead of replaying the first")
	case <-time.After(authAdmissionSettleWait):
	}

	close(release.release)

	firstSettled := <-firstAnswer
	secondSettled := <-secondAnswer

	require.NoError(t, firstSettled.err)
	require.NoError(t, secondSettled.err)
	require.Equal(t, firstSettled.result, secondSettled.result)

	fixture.brokerNode.mu.Lock()
	defer fixture.brokerNode.mu.Unlock()

	require.Len(t, fixture.brokerNode.authorizeCalls, 1)
}

// TestSessionCloseRefusesAnAuthorizeThatHasNotPublished holds an authorize
// between its session lookup and the publication of its record, and closes the
// session underneath it. Close takes its cleanup set from the published flows,
// so a flow published after that set was taken is one nothing reclaims: the
// broker process and its home outlive the session that owns them.
func TestSessionCloseRefusesAnAuthorizeThatHasNotPublished(t *testing.T) {
	fixture := newAuthFixture(t)
	params := fixture.authorizeParams(t, nil)

	release := holdFirstAuthToken(t)

	type answer struct {
		result any
		err    error
	}

	settled := make(chan answer, 1)

	go func() {
		result, err := fixture.broker.authorize(context.Background(), params)
		settled <- answer{result: result, err: err}
	}()

	<-release.arrived

	require.NoError(t, fixture.broker.closeSession(context.Background(), fixture.session.id))

	close(release.release)

	answered := <-settled
	require.Nil(t, answered.result)
	requireInvalidParams(t, answered.err, jsonFieldSessionID)

	fixture.brokerNode.mu.Lock()
	require.Empty(t, fixture.brokerNode.authorizeCalls)
	fixture.brokerNode.mu.Unlock()

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	require.Empty(t, fixture.broker.flows)
	require.Empty(t, fixture.broker.byID)
}

// TestRetiredAuthorizeRequestIDLeavesItsSuccessorAlone replays the idempotency
// key of a request a later authorize already replaced. Only the newest record
// can be answered verbatim, so an older key is unanswerable — and minting in
// its place destroys the live flow the key never named, which stops the code
// the operator is looking at from addressing anything.
func TestRetiredAuthorizeRequestIDLeavesItsSuccessorAlone(t *testing.T) {
	fixture := newAuthFixture(t)

	first := fixture.authorize(t, nil)
	second := fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})
	require.NotEqual(t, first.FlowID, second.FlowID)

	_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireInvalidParams(t, err, authFieldAuthorizeRequestID)

	require.Equal(t, authStatePending, fixture.status(t, second.FlowID).State)

	fixture.brokerNode.mu.Lock()
	require.Len(t, fixture.brokerNode.authorizeCalls, 2)
	fixture.brokerNode.mu.Unlock()

	// The tombstones are session-scoped, so closing the session that minted them
	// drops them with everything else the session could still be asked about.
	require.NoError(t, fixture.broker.closeSession(context.Background(), fixture.session.id))

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()

	require.Empty(t, fixture.broker.retired)
}

// TestAReloadedSessionIsAdmittedAgain pins the other end of the closed mark. A
// session/load after a session/close builds a fresh record under the same id,
// and the surface has to answer for it: the mark records a closed session, not
// a burnt id.
func TestAReloadedSessionIsAdmittedAgain(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.closeSession(context.Background(), fixture.session.id))

	_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireInvalidParams(t, err, jsonFieldSessionID)

	require.NoError(t, fixture.agent.storeStartedSession(fixture.session))
	require.NotEmpty(t, fixture.authorize(t, nil).FlowID)
}

// TestCredentialSlotGateRefusesACancelledLeg pins what a leg that never got the
// slot does: it answers, and it does not mutate the slot on its way out. Both
// mutators of one provider's credential are covered, because a leg that skipped
// the gate on a cancelled context would be exactly the leg the gate exists to
// exclude.
func TestCredentialSlotGateRefusesACancelledLeg(t *testing.T) {
	fixture := authSecretFixture(t)
	flow := fixture.authorize(t, nil)

	release, admitted := fixture.broker.slots.admit(context.Background(), "xai")
	require.True(t, admitted)

	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fixture.broker.callback(ctx, mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldMethod:     "0",
		authFieldFlowID:     flow.FlowID,
		authFieldInput:      "sk-secret",
	}))
	requireAuthFailure(t, err, authCauseTimeout)

	_, err = fixture.broker.disconnect(ctx, mustJSON(t, map[string]any{
		authFieldSessionID:         string(fixture.session.id),
		authFieldProviderID:        "xai",
		authFieldConnectionID:      "conn-1",
		authFieldBindingGeneration: int64(1),
	}))
	requireAuthFailure(t, err, authCauseTimeout)

	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()

	require.Empty(t, fixture.runtime.setAuthCalls)
	require.Empty(t, fixture.runtime.removedAuth)
}

// TestALedgerIntentThatNeverGotTheSlotWritesNothing pins the authorize half of
// the slot gate. The leg cancels between taking its key gate and reaching the
// slot, so the slot is the admission it loses: it claims no revision, records
// no intent, and drives no native mint.
func TestALedgerIntentThatNeverGotTheSlotWritesNothing(t *testing.T) {
	fixture := newAuthFixture(t)

	release, admitted := fixture.broker.slots.admit(context.Background(), "xai")
	require.True(t, admitted)

	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	original := authRandRead
	authRandRead = func(value []byte) (int, error) {
		cancel()

		return original(value)
	}

	t.Cleanup(func() { authRandRead = original })

	_, err := fixture.broker.authorize(ctx, fixture.authorizeParams(t, nil))
	requireAuthFailure(t, err, authCauseTimeout)

	_, ok, readErr := fixture.broker.ledger.read("xai")
	require.NoError(t, readErr)
	require.False(t, ok)

	fixture.brokerNode.mu.Lock()
	defer fixture.brokerNode.mu.Unlock()

	require.Empty(t, fixture.brokerNode.authorizeCalls)
}

// TestInstallRefusesABindingTheLedgerHasAlreadyLeft pins the pre-write half of
// the lineage check. The post-hoc confirmation refusal is not enough on its
// own: it correctly declines to record the stale binding, but by then the
// credential is resident under an entry that names something else.
func TestInstallRefusesABindingTheLedgerHasAlreadyLeft(t *testing.T) {
	fixture := authSecretFixture(t)
	flow := fixture.authorize(t, nil)

	_, err := fixture.disconnect(t, "conn-1", 1)
	require.NoError(t, err)

	_, err = fixture.callback(t, flow.FlowID, "0", "sk-secret")
	requireAuthFailure(t, err, authCauseBindingConflict)

	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()

	require.Empty(t, fixture.runtime.setAuthCalls)
}

// TestInstallAnswersAnUnreadableLedger pins that the pre-write lineage read is
// load-bearing: a lineage this leg cannot read is one it cannot claim to still
// name, so nothing is written.
func TestInstallAnswersAnUnreadableLedger(t *testing.T) {
	fixture := authSecretFixture(t)
	flow := fixture.authorize(t, nil)

	ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("io") }

	t.Cleanup(func() { ledgerReadFile = os.ReadFile })

	_, err := fixture.callback(t, flow.FlowID, "0", "sk-secret")
	requireAuthFailure(t, err, authCauseProcess)

	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()

	require.Empty(t, fixture.runtime.setAuthCalls)
}

// TestProbeDeclinesAFlowAnotherLegHolds pins the poll's half of the claim: a
// status call that arrives while the owner's callback is driving the same flow
// reports the cached state rather than starting a native read of its own.
func TestProbeDeclinesAFlowAnotherLegHolds(t *testing.T) {
	fixture := newAuthFixture(t)
	flow := fixture.authorize(t, nil)

	var reads atomic.Int64

	fixture.brokerNode.storedAuthFunc = func(string) { reads.Add(1) }

	fixture.broker.mu.Lock()
	fixture.broker.byID[flow.FlowID].claimed = true
	fixture.broker.mu.Unlock()

	require.Equal(t, authStatePending, fixture.status(t, flow.FlowID).State)
	require.Zero(t, reads.Load())
}

// TestAdmissionGatesAreDroppedOnlyWhenNobodyHoldsThem pins that a settled leg
// leaves no gate behind. The keys are unbounded over the life of an agent that
// outlives every session it serves, so a gate that survives its last holder is
// a leak, and one dropped while a holder remains is worse than a leak: the next
// leg mints a fresh gate and runs beside the leg still holding the old one.
func TestAdmissionGatesAreDroppedOnlyWhenNobodyHoldsThem(t *testing.T) {
	fixture := authSecretFixture(t)

	flow := fixture.authorize(t, nil)

	_, err := fixture.callback(t, flow.FlowID, "0", "sk-secret")
	require.NoError(t, err)

	_, err = fixture.disconnect(t, "conn-1", 1)
	require.NoError(t, err)

	fixture.broker.admissions.mu.Lock()
	require.Empty(t, fixture.broker.admissions.gates)
	fixture.broker.admissions.mu.Unlock()

	fixture.broker.slots.mu.Lock()
	defer fixture.broker.slots.mu.Unlock()

	require.Empty(t, fixture.broker.slots.gates)
}

// TestAReopenedSessionCannotSlipPastAHeldGate is the reason nothing may delete
// a gate out from under its holder. A session that closes and is loaded again
// admits legs under the same key, and if the close dropped the gate entry the
// new leg mints its own and runs concurrently with the one still inside the
// admission it is supposed to be queued behind.
func TestAReopenedSessionCannotSlipPastAHeldGate(t *testing.T) {
	fixture := newAuthFixture(t)
	params := fixture.authorizeParams(t, nil)

	var inside, peak atomic.Int64

	hold := authTokenHold{arrived: make(chan struct{}), release: make(chan struct{})}
	original := authRandRead

	var held atomic.Bool

	authRandRead = func(value []byte) (int, error) {
		recordPeak(inside.Add(1), &peak)

		if held.CompareAndSwap(false, true) {
			close(hold.arrived)
			<-hold.release
		}

		inside.Add(-1)

		return original(value)
	}

	t.Cleanup(func() { authRandRead = original })

	first := make(chan struct{})

	go func() {
		defer close(first)

		_, _ = fixture.broker.authorize(context.Background(), params)
	}()

	<-hold.arrived

	require.NoError(t, fixture.broker.closeSession(context.Background(), fixture.session.id))
	require.NoError(t, fixture.agent.storeStartedSession(fixture.session))

	second := make(chan struct{})

	go func() {
		defer close(second)

		_, _ = fixture.broker.authorize(context.Background(), params)
	}()

	time.Sleep(authAdmissionSettleWait)
	close(hold.release)
	<-first
	<-second

	require.Equal(t, int64(1), peak.Load())
}

func recordPeak(current int64, peak *atomic.Int64) {
	for {
		top := peak.Load()
		if current <= top || peak.CompareAndSwap(top, current) {
			return
		}
	}
}

// TestDisconnectAndAuthorizeCannotClobberOneLedgerEntry holds a disconnect
// between reading the provider's record and writing its generation bump, and
// runs a whole authorize underneath it. Both are read-modify-writes of one
// ledger entry, so both have to hold the same credential-slot gate a login and
// a removal already hold: ungated, the copy disconnect read before the new
// login existed is written back over it, erasing the revision the login is
// bound to and leaving that login unable to ever confirm.
func TestDisconnectAndAuthorizeCannotClobberOneLedgerEntry(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", Revision: 1, BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	arrived := make(chan struct{})
	release := make(chan struct{})
	original := authNow

	var armed, held atomic.Bool

	// disconnect stamps the bump it is about to write between its read and that
	// write, which is the one point inside the read-modify-write that holds no
	// ledger mutex.
	authNow = func() time.Time {
		if armed.Load() && held.CompareAndSwap(false, true) {
			close(arrived)
			<-release
		}

		return original()
	}

	t.Cleanup(func() { authNow = original })

	disconnected := make(chan error, 1)

	armed.Store(true)

	go func() {
		_, err := fixture.disconnect(t, "conn-1", 1)
		disconnected <- err
	}()

	<-arrived

	authorized := make(chan struct{})

	go func() {
		defer close(authorized)

		_, _ = fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	}()

	time.Sleep(authAdmissionSettleWait)
	close(release)

	require.NoError(t, <-disconnected)
	<-authorized

	record, ok, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(2), record.Revision)
	require.Equal(t, int64(2), record.BindingGeneration)
	require.Equal(t, authLedgerIntent, record.State)
}

// authTokenHold parks the first flow-id mint, which is the one point every
// authorize passes between reading the current record and publishing its own.
type authTokenHold struct {
	arrived chan struct{}
	release chan struct{}
}

func holdFirstAuthToken(t *testing.T) authTokenHold {
	t.Helper()

	hold := authTokenHold{arrived: make(chan struct{}), release: make(chan struct{})}
	original := authRandRead

	var held atomic.Bool

	authRandRead = func(value []byte) (int, error) {
		if held.CompareAndSwap(false, true) {
			close(hold.arrived)
			<-hold.release
		}

		return original(value)
	}

	t.Cleanup(func() { authRandRead = original })

	return hold
}
