package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	authoritativeDeliveryCapacity = 256
	rawDeliveryCapacity           = 64
	deliveryInterruptGrace        = 100 * time.Millisecond
)

var errAuthoritativeDeliveryPanic = errors.New("authoritative session delivery panicked")

// deliveryOutcome is what became of one authoritative notification.
//
// handed is the difference between a fact that may be in the client's hands and
// one that provably never left this process. The worker sets it immediately
// before the transport send, so a job it drained without ever reaching that send
// — a queue emptied behind a preceding write failure, or the worker's own context
// ending — reports the failure with handed false. Only the send itself is
// ambiguous: it may reach the client and still report an error, so a handed
// notification is one this process can no longer take back.
type deliveryOutcome struct {
	err    error
	handed bool
}

type deliveryReceipt <-chan deliveryOutcome

type authoritativeDelivery struct {
	notification acp.SessionNotification
	done         chan deliveryOutcome
	onFailure    func(error)
	onDelivered  func()
}

type rawDelivery struct {
	payload map[string]any
	done    chan error
}

// sessionDelivery owns transport writes for one session. Typed updates and raw
// events use separate bounded ordered lanes, so a raw writer can neither stand
// ahead of authoritative state nor consume its capacity.
type sessionDelivery struct {
	agent *Agent
	id    acp.SessionId

	cancel context.CancelFunc

	mu        sync.Mutex
	started   bool
	closed    bool
	failure   error
	typed     chan authoritativeDelivery
	raw       chan rawDelivery
	typedDone chan struct{}
	rawDone   chan struct{}
}

func newSessionDelivery(agent *Agent, id acp.SessionId) *sessionDelivery {
	delivery := &sessionDelivery{
		agent: agent, id: id,
		typed:     make(chan authoritativeDelivery, authoritativeDeliveryCapacity),
		raw:       make(chan rawDelivery, rawDeliveryCapacity),
		typedDone: make(chan struct{}), rawDone: make(chan struct{}),
	}

	return delivery
}

func (d *sessionDelivery) startLocked() {
	if d.started {
		return
	}

	d.started = true
	workerCtx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel

	go d.runTyped(workerCtx)
	go d.runRaw(workerCtx)
}

func (d *sessionDelivery) enqueueUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
	onFailures ...func(error),
) (deliveryReceipt, error) {
	return d.enqueueUpdateCommitted(ctx, notification, nil, onFailures...)
}

func (d *sessionDelivery) enqueueUpdateCommitted(
	ctx context.Context,
	notification acp.SessionNotification,
	onDelivered func(),
	onFailures ...func(error),
) (deliveryReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil, errors.New("session delivery is closed")
	}

	if d.failure != nil {
		return nil, d.failure
	}

	d.startLocked()

	done := make(chan deliveryOutcome, 1)

	job := authoritativeDelivery{notification: notification, done: done, onDelivered: onDelivered}
	if len(onFailures) > 0 {
		job.onFailure = onFailures[0]
	}

	select {
	case d.typed <- job:
		return done, nil
	default:
		return nil, errors.New("authoritative session delivery queue is full")
	}
}

func waitDelivery(ctx context.Context, receipt deliveryReceipt) error {
	return waitDeliveryOutcome(ctx, receipt).err
}

// waitDeliveryOutcome waits for the whole outcome rather than only its failure.
// It exists for the one caller that must know whether a failed notification was
// handed to the transport: everything else only ever acts on the error.
func waitDeliveryOutcome(_ context.Context, receipt deliveryReceipt) deliveryOutcome {
	if receipt == nil {
		return deliveryOutcome{}
	}

	// Once a fact is accepted, caller cancellation cannot withdraw its ordered
	// delivery. The worker's own bounded context remains its interrupt authority.
	return <-receipt
}

func (d *sessionDelivery) runTyped(ctx context.Context) {
	defer close(d.typedDone)

	for {
		select {
		case <-ctx.Done():
			d.failQueued(ctx.Err())

			return
		case job := <-d.typed:
			handed, err := d.deliverAuthoritative(ctx, job)
			if err != nil {
				failure := d.latchFailure(err)

				job.done <- deliveryOutcome{err: failure, handed: handed}

				d.runFailureCallback(job.onFailure, failure)

				d.failQueued(failure)

				return
			}

			job.done <- deliveryOutcome{handed: true}
		}
	}
}

// deliverAuthoritative writes one notification and reports whether it reached the
// transport. handed is a named return set through the write itself so that a
// panic raised inside the send is still reported as handed: the transport had the
// notification before it exploded, and claiming otherwise would let the turn deny
// a fact the client may hold.
func (d *sessionDelivery) deliverAuthoritative(
	ctx context.Context,
	job authoritativeDelivery,
) (handed bool, err error) {
	defer func() {
		if recover() != nil {
			err = errAuthoritativeDeliveryPanic

			d.terminalizePanic("authoritative session delivery panicked")
		}
	}()

	if err := d.writeUpdate(ctx, job, &handed); err != nil {
		return handed, err
	}

	if job.onDelivered != nil {
		job.onDelivered()
	}

	return true, nil
}

func (d *sessionDelivery) latchFailure(cause error) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failure == nil {
		if errors.Is(cause, errAuthoritativeDeliveryPanic) {
			d.failure = fmt.Errorf("authoritative session update delivery failed: %w", errAuthoritativeDeliveryPanic)
		} else {
			d.failure = errors.New("authoritative session update delivery failed")
		}
	}

	return d.failure
}

func (d *sessionDelivery) runFailureCallback(callback func(error), failure error) {
	if callback == nil {
		return
	}

	panicked := false

	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()

		callback(failure)
	}()

	if panicked {
		d.terminalizePanic("authoritative delivery failure callback panicked")
	}
}

func (d *sessionDelivery) terminalizePanic(cause string) {
	if d == nil || d.agent == nil {
		return
	}

	defer func() { _ = recover() }()

	current, err := d.agent.session(d.id)
	if err != nil {
		return
	}

	binding := current.currentIncarnation()
	current.failNativeIncarnation(binding, errors.New(cause))
}

// writeUpdate hands one notification to the transport. It marks handed at the
// last instruction before the send, so a session with no connection left to send
// on reports a failure nothing could have received.
func (d *sessionDelivery) writeUpdate(
	ctx context.Context,
	job authoritativeDelivery,
	handed *bool,
) error {
	conn := d.agent.connection()
	if conn == nil {
		return errors.New("session update delivery has no ACP connection")
	}

	writeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	*handed = true

	return conn.SessionUpdate(writeCtx, job.notification)
}

// failQueued empties the queue behind a worker that is stopping. Not one of these
// jobs was ever handed to the transport — the worker's context ended, or a
// preceding write failed and latched the lane — so each reports a failure whose
// notification provably never left this process.
func (d *sessionDelivery) failQueued(err error) {
	for {
		select {
		case job := <-d.typed:
			job.done <- deliveryOutcome{err: err}

			d.runFailureCallback(job.onFailure, err)
		default:
			return
		}
	}
}

func (d *sessionDelivery) enqueueRaw(ctx context.Context, payload map[string]any) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()

		return errors.New("session delivery is closed")
	}

	d.startLocked()
	rawDone := d.rawDone
	d.mu.Unlock()

	done := make(chan error, 1)
	select {
	case d.raw <- rawDelivery{payload: payload, done: done}:
	case <-rawDone:
		return errors.New("raw session delivery stopped")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-rawDone:
		return errors.New("raw session delivery stopped")
	}
}

func (d *sessionDelivery) flushRaw(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()

		return errors.New("session delivery is closed")
	}

	d.startLocked()

	done := make(chan error, 1)
	select {
	case d.raw <- rawDelivery{done: done}:
		d.mu.Unlock()
	case <-ctx.Done():
		d.mu.Unlock()

		return ctx.Err()
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *sessionDelivery) runRaw(ctx context.Context) {
	defer func() {
		d.failRawQueued(errors.New("raw session delivery stopped"))
		close(d.rawDone)
	}()

	var sequence int64

	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.raw:
			if job.payload == nil {
				if job.done != nil {
					job.done <- nil
				}

				continue
			}

			err := d.deliverRaw(ctx, job.payload, sequence+1)
			if err == nil {
				sequence++
			}

			if job.done != nil {
				job.done <- err
			}
		}
	}
}

func (d *sessionDelivery) deliverRaw(ctx context.Context, payload map[string]any, sequence int64) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("raw session delivery panicked")
		}
	}()

	conn := d.agent.connection()
	if conn == nil {
		return errors.New("raw delivery has no ACP connection")
	}

	payload[jsonFieldSequence] = sequence

	capped, err := capRawEventPayload(payload)
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	return conn.NotifyExtension(writeCtx, RawEventMethod, capped)
}

func (d *sessionDelivery) failRawQueued(err error) {
	for {
		select {
		case job := <-d.raw:
			if job.done != nil {
				job.done <- err
			}
		default:
			return
		}
	}
}

func (d *sessionDelivery) close() {
	if d == nil {
		return
	}

	d.mu.Lock()
	if d.closed {
		started := d.started
		d.mu.Unlock()

		if !started {
			return
		}

		if !d.waitWorkers(deliveryInterruptGrace) {
			d.agent.interruptConnection()
			_ = d.waitWorkers(closeTimeout)
		}

		return
	}

	d.closed = true

	started := d.started

	if !started {
		d.mu.Unlock()

		return
	}

	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()

	if d.waitWorkers(deliveryInterruptGrace) {
		return
	}

	d.agent.interruptConnection()
	_ = d.waitWorkers(closeTimeout)
}

func (d *sessionDelivery) waitWorkers(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	typedDone, rawDone := d.typedDone, d.rawDone
	for typedDone != nil || rawDone != nil {
		select {
		case <-typedDone:
			typedDone = nil
		case <-rawDone:
			rawDone = nil
		case <-timer.C:
			return false
		}
	}

	return true
}
