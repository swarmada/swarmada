/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package safety delivers emergency stops over the SafetyStream and tracks their
// confirmed outcome (RFC-0001 #safety-emergency-stop-protocol). The cardinal invariant: a robot is treated
// as STOPPED only when the Fleet Adapter CONFIRMS it via EstopAck.state=STOPPED —
// never inferred from silence, a dropped message, or a timeout. Anything the
// control plane cannot confirm resolves to Failed (escalate), never Stopped.
package safety

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ErrUndelivered means the estop could not be confirmed via the SafetyStream
// (no connected stream, send failure, or no EstopAck). The robot is left Failed
// and MUST be escalated (edge/manual) — it is NOT stopped.
var ErrUndelivered = errors.New("estop not confirmed via SafetyStream; escalate")

// ErrStateUnwritten means the estop's outcome (Stopped, Failed) was reached but writing it
// to Robot.status.estopState failed. The Result still says what the adapter answered; the
// caller MUST NOT record the stop as issued, and retries it, because nothing on the Robot
// says it is stopped (RFC-0001 #safety-estop-state-machine).
var ErrStateUnwritten = errors.New("estop outcome not written to Robot.status.estopState")

// Default timing (RFC-0001 #safety-timing-requirements and #safety-estop-state-machine).
const (
	defaultSLAThreshold    = 500 * time.Millisecond
	defaultConfirmTimeout  = 10 * time.Second
	defaultDeliveryTimeout = 2 * time.Second
)

// Result is the outcome of a TriggerEstop call.
type Result struct {
	// Delivered is true if the estop reached the adapter and at least one ack came.
	Delivered bool
	// Confirmed is true ONLY if an EstopAck.state=STOPPED was received.
	Confirmed bool
	// State is the resulting Robot.status.estopState.
	State fleetv1.RobotEstopState
	// Latency is the send→first-ack round trip (valid when Delivered).
	Latency time.Duration
	// LatencyViolation is true when Latency exceeded the 500ms SLA.
	LatencyViolation bool
}

// Dispatcher pushes estops down live SafetyStreams and records confirmed outcomes.
type Dispatcher struct {
	Client   client.Client
	Recorder record.EventRecorder
	// APIReader reads the API server directly, bypassing the manager's cache. The
	// estopState write compares against the stored state through it, because the cache
	// may not yet hold another trigger's write made a moment earlier. Nil falls back to
	// Client.
	APIReader client.Reader
	// Telemetry is what the robot's own telemetry last said: when its last frame was
	// received, the phase it last reported, and whether that report has reached
	// Robot.status. An operator's resume confirmation is refused until the robot has
	// sent a frame since the clear, while its most recent report is not yet written, and
	// while its own report puts it in Error or Offline (RFC-0001
	// #safety-estop-state-machine). Nil refuses every confirmation, and a cleared robot
	// stays Resuming.
	Telemetry TelemetryClock
	// DeriveCapabilities computes a robot's capability list from its current status —
	// the same derivation the Robot reconciler writes to status.capabilities. The resume
	// confirmation uses it to check the capabilities the reported hardware supports now,
	// because the stored list is written later, by a reconcile nothing orders before
	// this one; the stop uses it to record which capabilities were Active when it was
	// taken (status.estopBaseline). Nil refuses every confirmation, and a cleared robot
	// stays Resuming.
	DeriveCapabilities func(*fleetv1.Robot) []fleetv1.CapabilityStatusEntry
	// Audit seals ESTOP_LATENCY_VIOLATION (RFC-0001 #safety-required-events) into the tamper-evident chain.
	// Nil disables recording; estop delivery is unaffected.
	Audit audit.Recorder
	// Term reports the term of the leadership this process holds, and whether it holds it
	// (leadership.State.Term), set in the manager. A process that no longer leads writes no
	// stop outcome (S2 to S4), and records no metric or audit entry for a stop's answer: the
	// leader that took over derives the stops from their carriers. Sending a stop is not gated
	// on it: a stop is safe whoever sends it. Nil (the package's own tests) leads with term 0.
	Term func() (term uint64, leading bool)
	// Fence sends each stop's fence on the robot's ControlStream (command.Dispatcher), in order with
	// its AssignActions, and forgets the robot's stops in force once a clear has returned it to
	// Resuming. Nil sends no fence (the package's own tests; a manager without ControlStream).
	Fence FenceSender

	now             func() time.Time
	slaThreshold    time.Duration
	confirmTimeout  time.Duration
	deliveryTimeout time.Duration

	mu        sync.Mutex
	streams   map[string]controlstream.SafetySender // adapterKey → live sender
	pending   map[pendingKey]*pendingEstop          // (namespace, wire robot_id, estopID) → waiter
	orders    map[string]*estopOrder                // robotEstopKey → estop write ordering
	idCounter uint64
	// inflight is each robot's stops sent and not yet recorded (robotEstopKey → stop ID): stops in
	// this process's own state, which a clear's guard and S6's guard read (clear.go).
	inflight map[string]map[string]*stopRecord
	// contracts is the contract version each adapter last reported on its ControlStream (adapterKey).
	contracts map[string]string
	// removed is, per robot, the IDs of the entries this process removed from status.stops
	// (rememberedKey → oldest first), so a later report of one is answered as a cleared stop
	// (removed.go).
	removed map[string][]removedStop
	// issued is the stop IDs this process issued: a recorded stop among them is this process's own,
	// never a predecessor's to run out (takeover.go).
	issued map[string]struct{}
	// kept is, per adapter (adapterKey), the reports of remembered IDs that arrived before the
	// adapter's connection was known here to be at 1.4.0 (removed.go).
	kept map[string][]keptReport
}

// estopOrder is a robot's estop write-ordering record: what makes a trigger and a
// clear issued concurrently resolve by the order the two EVENTS were issued rather
// than by the order their patches happen to reach the API server.
//
// The ordering is deliberately one-sided. A stop is never gated on it — estop
// carries no fence precisely so a stale token can never refuse a stop (RFC-0001 #safety-emergency-stop-protocol) —
// and only the release direction consults it. The one exception is not a gate on a
// stop: an older trigger's outcome is not written over a newer trigger's confirmed
// Stopped (stoppedBy), because the robot is already confirmed at rest.
type estopOrder struct {
	// issued is the last epoch handed out for this robot. Every estop event takes
	// one at the moment it is issued, a trigger and a clear alike.
	issued uint64
	// lastStop is the epoch of the most recent TRIGGER. A clear whose own epoch is
	// older than this was issued BEFORE that trigger, so it is not the later event
	// and must not release the stop.
	lastStop uint64
	// lastClear is the epoch of the most recent CLEAR. issued alone cannot say whether
	// a clear followed a trigger, because a resume confirmation takes an epoch too. A
	// trigger with a clear after it is no longer wanted, and is not re-sent.
	lastClear uint64
	// stoppedBy is the epoch of the trigger whose confirmed Stopped is the state this
	// dispatcher last recorded for the robot, and 0 once it writes any other state. An
	// outcome of an OLDER trigger is not written over it: a confirmed stop is never
	// discarded (RFC-0001 #safety-estop-state-machine), and an older trigger's timeout
	// says nothing about a stop a newer one confirmed. Guarded by write, not by mu.
	stoppedBy uint64
	// write serialises check-and-patch for this robot, so a trigger cannot register
	// in the window between a clear's ordering check and its write. It covers the
	// status write only — never the SafetyStream send — so it cannot add latency to
	// a stop.
	write sync.Mutex

	// resumeArmed reports whether this robot is inside the post-clear supervised
	// window and a baseline for leaving it has been established.
	resumeArmed bool
	// resumeSince is the dispatcher's clock at the moment the window was armed. An
	// operator's confirmation is refused until a telemetry frame received STRICTLY
	// LATER has arrived: the projection standing at the moment of the clear is the
	// pre-estop one, and the confirmation's checks would otherwise read it.
	resumeSince time.Time
	// resumeAdopted reports that this leader found the robot in Resuming and did not
	// accept its clear. The baseline is then not resumeSince but the first frame this
	// process received from the robot (TelemetryClock.FirstFrame), which arrived after its
	// takeover: a process is served adapter streams only while it leads (RFC-0001
	// #safety-robot-stop-table). Like the rest of this record it assumes one leadership per
	// process, which holds because the manager stops when the election Lease is lost.
	resumeAdopted bool
}

// pendingKey identifies one outstanding estop by what the control plane wrote when it sent
// it: the namespace, the robot_id on the envelope, and the estop_id. An ack resolves an
// estop only by all three, so naming an estop_id is not enough to confirm it.
//
// robot is the WIRE robot_id (robotid.Wire), not the object name: it is the id the
// adapter announced, the only one it knows the robot by, and the one its ack names. For a
// robot admitted under another name the two differ, and an ack naming the object name
// would be about a robot the adapter never announced.
type pendingKey struct{ namespace, robot, estopID string }

// pendingEstop is an outstanding estop's waiter, the adapter it was sent to, and the
// SafetyStream it went down. Every field is guarded by Dispatcher.mu.
type pendingEstop struct {
	adapter string
	sender  controlstream.SafetySender
	ch      chan *fav1.EstopAck

	// robot is the Robot's object name, which the estop ordering record is keyed by. The
	// robot_id the estop went out under is key.robot.
	robot string

	// key is the (namespace, robot, estop_id) the estop is awaited under. The estop_id is the
	// stop ID, minted once at issue; every re-send carries it.
	key pendingKey
	// estop is the estop as sent. A re-send carries the same reason, issuer and issue
	// time under the new estop_id.
	estop *fav1.Estop
	// epoch is the trigger's estop epoch, which says whether the estop is still wanted.
	epoch uint64
	// deadline is when the window the waiter is in (delivery, then confirmation) ends;
	// zero until the waiter has started it.
	deadline time.Time
	// settled is set once the estop is resolved: an ack that resolves it (STOPPED or
	// FAILED) has been routed, or the waiter has given up on it.
	settled bool
	// taken is set on a stream-open re-send's waiter (resendHeldTo), and closed when a trigger that
	// runs the same recorded stop out takes its stop ID (register): that waiter then stops, and
	// records nothing.
	taken chan struct{}
}

// robotEstopKey identifies a robot's estop ordering record.
func robotEstopKey(namespace, name string) string { return namespace + "/" + name }

// New builds a Dispatcher with the RFC-0001 #safety-emergency-stop-protocol default timings.
func New(c client.Client, recorder record.EventRecorder) *Dispatcher {
	return &Dispatcher{
		Client:          c,
		Recorder:        recorder,
		slaThreshold:    defaultSLAThreshold,
		confirmTimeout:  defaultConfirmTimeout,
		deliveryTimeout: defaultDeliveryTimeout,
		streams:         map[string]controlstream.SafetySender{},
		pending:         map[pendingKey]*pendingEstop{},
		orders:          map[string]*estopOrder{},
	}
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// deliveryTimeoutFor resolves the per-adapter ACK wait for a namespace from
// spec.estop.delivery.perAdapterTimeoutMs (ADR-0016). It FAILS SAFE to the
// dispatcher's built-in default on any unreadable/absent config or a non-positive
// value, so a config gap never lengthens or shortens the safety-critical delivery
// window beyond the vetted default. Never relaxes confirmation semantics.
func (d *Dispatcher) deliveryTimeoutFor(ctx context.Context, namespace string) time.Duration {
	var list fleetv1.SwarmadaConfigList
	if err := d.Client.List(ctx, &list, client.InNamespace(namespace)); err == nil && len(list.Items) > 0 {
		if ms := list.Items[0].Spec.Estop.Delivery.PerAdapterTimeoutMs; ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return d.deliveryTimeout
}

func adapterKey(namespace, adapter string) string { return namespace + "/" + adapter }

// RegisterStream records a live SafetyStream for an adapter identity and returns a
// deregister func to call when the stream closes. An unverified identity is not
// registered (nothing may be pushed to an unauthenticated stream).
//
// One identity has one live SafetyStream. A stream registered under an identity that
// already has one replaces it, and estops go down the newer stream from that moment; the
// server closes the replaced stream once this call has returned, so no estop falls
// between the two (controlstream.Server.SafetyStream). The replaced stream's deregister
// leaves the newer registration in place.
//
// An estop still awaiting its ack on the replaced stream could never be confirmed: that
// stream is about to close, and an ack on this one names an estop that did not go down it.
// So each such estop is re-sent down this stream before this call returns, under a new
// estop_id bound to this stream (resendOnto).
//
// A stop that is no longer awaited is re-sent too: every robot of the adapter whose
// status.estopState is Stopping, Stopped or Failed is sent its stop once down this stream
// (resendHeld), unless an estop still awaited for it was just moved onto the stream. A
// robot that reconnects under a stop is told again that the stop stands.
func (d *Dispatcher) RegisterStream(identity controlstream.TLSIdentity, sender controlstream.SafetySender) func() {
	if !identity.Verified {
		return func() {}
	}
	key := adapterKey(identity.Namespace, identity.AdapterName)
	d.mu.Lock()
	replaced := d.streams[key]
	d.streams[key] = sender
	resend := d.rebindLocked(identity, replaced, sender)
	d.mu.Unlock()
	d.resendOnto(sender, resend)
	moved := make(map[string]bool, len(resend))
	for _, m := range resend {
		moved[m.robotID] = true
	}
	go d.resendHeld(identity, sender, moved)
	return func() {
		d.mu.Lock()
		if d.streams[key] == sender {
			delete(d.streams, key)
		}
		d.mu.Unlock()
	}
}

// RouteAck delivers an EstopAck to the waiting TriggerEstop. robotID is the robot_id the
// ack names, which is a wire robot_id. It resolves only the estop sent under robotID in
// identity's namespace under this estop_id, and only when the ack
// arrived on the SafetyStream the estop went down (from), under the adapter it was sent to
// (RFC-0001 #fleet-adapter-protocol-the-controlstream-service): a confirmed stop is proof the robot is at
// rest, so it is taken only from the stream that carried the estop, for the robot it was
// sent for. A second process under the same certificate did not receive the estop, and
// its STOPPED is not evidence of anything, whatever estop_id it names. The robot_id is the
// one the control plane wrote on the envelope when it sent the estop; the ack only has to
// match it.
//
// An ack for no pending estop (late, or never issued) is dropped and returns nil. An ack
// for a pending estop that did not come back the way it went is dropped and returns an
// error wrapping controlstream.ErrMisroutedAck, which the server records as a denial;
// the estop keeps waiting for its own stream, and resolves Failed if that stream never
// answers. A stop re-sent down the stream that replaced the one it went down keeps its stop
// ID and is bound to the new stream: an ack with that ID on the replaced stream confirms
// nothing. It never blocks the SafetyStream receive loop (buffered channel).
func (d *Dispatcher) RouteAck(identity controlstream.TLSIdentity, from controlstream.SafetySender, robotID string, ack *fav1.EstopAck) error {
	if ack == nil || !identity.Verified || robotID == "" {
		return nil
	}
	pk := pendingKey{namespace: identity.Namespace, robot: robotID, estopID: ack.GetEstopId()}
	d.mu.Lock()
	p := d.pending[pk]
	if p == nil {
		// Not awaited. An ID whose entry was removed and is remembered for the robot is a report
		// of a cleared stop, answered with its fence and its EstopClear (removed.go). Any other is
		// dropped: a reported stop is the end-state work.
		rm, remembered := d.rememberedLocked(identity.Namespace, robotID, ack.GetEstopId())
		d.mu.Unlock()
		if remembered {
			go d.answerRemoved(identity, from, robotID, rm)
		}
		return nil
	}
	if p.adapter != identity.AdapterName || p.sender != from {
		adapter := p.adapter
		d.mu.Unlock()
		return fmt.Errorf("%w: estop %q for robot %q was sent to adapter %q on another SafetyStream",
			controlstream.ErrMisroutedAck, ack.GetEstopId(), robotID, adapter)
	}
	if st := ack.GetState(); st == fav1.EstopState_ESTOP_STATE_STOPPED || st == fav1.EstopState_ESTOP_STATE_FAILED {
		p.settled = true
	}
	ch := p.ch
	d.mu.Unlock()
	select {
	case ch <- ack:
	default:
	}
	return nil
}

// TriggerEstop pushes an estop to robotID's adapter and resolves its confirmed
// state. It writes Robot.status.estopState=Stopped ONLY on a confirmed
// EstopAck.state=STOPPED; a dropped estop, silence, a send failure, an adapter
// FAILED, or STOPPING-without-STOPPED all resolve to Failed (escalate), never
// Stopped. Estop is never fenced — no lease/priority gate.
//
// robotID is the Robot's object name. The estop goes out under the robot's wire robot_id
// (robotid.Wire), resolved once here from the Robot just read, and the estop is awaited
// under that id for its whole life, re-sends included. If the annotation is re-pointed
// while the estop is in flight, an ack under the new id matches nothing, and one under the
// old id names a robot_id no Robot carries any more, which the SafetyStream authorizer
// refuses; the estop then resolves Failed at the end of its window, never Stopped.
//
// The stop is sent before anything is written (row S1): its stop ID is minted, the waiter
// registered and the Estop sent, and only then is the stop recorded in Robot.status.stops,
// in a goroutine of its own, so neither a slow record write nor its retries delay the send
// or move the measured latency. The record gates nothing. carrier names the object whose
// stop this is; the zero Carrier on a robot-scope stop is the robot itself. The Robot is read
// from the manager's cache, never past it: S1 awaits nothing before the send, so a robot the
// cache has not seen yet is an error before any send, which the caller's requeue retries.
func (d *Dispatcher) TriggerEstop(ctx context.Context, namespace, robotID, reason, issuedBy string,
	scope metrics.EstopScope, carrier Carrier) (Result, error) {
	// This trigger's estop epoch, taken before the lookup: it marks when the stop was
	// issued, which is what a concurrent clear is ordered against. Taking
	// it cannot fail and cannot block, so it is never in the stop's way.
	epoch := d.issueEstopEpoch(robotEstopKey(namespace, robotID), true)
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return Result{}, fmt.Errorf("estop: robot %q lookup: %w", robotID, err)
	}

	adapter := robot.Spec.Adapter.Name
	wireID := robotid.Wire(robot)
	// A stop of this carrier that a predecessor recorded and this process did not issue is not
	// issued again with fresh windows: its recorded windows are run out (row S4).
	if st, ok := d.predecessorStop(robot, scope, carrier); ok {
		return d.runOutRecorded(ctx, robot, st, reason, issuedBy, scope, epoch)
	}
	sentAt, sentWall := d.clock(), time.Now()
	stopID := NewStopID()
	d.markIssued(stopID)
	rec := d.newStopRecord(robot, stopID, scope, carrier, sentAt)
	rec.epoch = epoch
	defer rec.finish()
	// In this process's own state before the send, until its record lands: a clear's guard reads it.
	d.trackInflight(robotEstopKey(namespace, robotID), rec)
	// The stream is looked up and the estop registered on it in one step, so a stream
	// registered in between cannot miss it: either the estop is on the replaced stream
	// when the new one registers, and is re-sent, or it goes down the new one.
	sender, p, seq := d.register(namespace, robotID, wireID, adapter, stopID, epoch,
		&fav1.Estop{Reason: reason, IssuedBy: issuedBy, IssuedAtMs: sentAt.UnixMilli()})
	d.sendFence(robot, stopID, &fav1.Estop{Reason: reason, IssuedBy: issuedBy, IssuedAtMs: sentAt.UnixMilli()})
	if sender == nil {
		// No SafetyStream to the adapter — cannot confirm via the control plane. The stop
		// stands all the same, its fence is on its way, and it is recorded.
		go d.recordStop(rec)
		werr := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "no SafetyStream to adapter "+robot.Spec.Adapter.Name, epoch)
		return Result{State: fleetv1.RobotEstopFailed}, joinUnwritten(ErrUndelivered, werr)
	}
	defer d.deregister(p)

	err := sender.Send(estopMessage(p.key.robot, seq, p.estop, p.key.estopID))
	// Sent: now, and only now, the stop is recorded (row S1), in its own goroutine.
	go d.recordStop(rec)
	if err != nil && d.settleUnlessResent(p, sender) {
		werr := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "estop send failed: "+err.Error(), epoch)
		return Result{State: fleetv1.RobotEstopFailed}, joinUnwritten(fmt.Errorf("estop send: %w", err), werr)
	}
	// A send that failed after the estop was re-sent down the stream that replaced this
	// one is not the end of it: the estop waits for its ack there, in the same window.
	return d.await(ctx, robot, p, scope, epoch, sentAt, sentWall, nil, rec.acknowledged)
}

// await runs a sent stop's windows and writes its outcome (rows S2 to S4). The delivery window runs
// from the send whose answer it waits for: a first send, at the sentAt its record stores, or a
// re-send. sentFrom is that send on the dispatcher's clock, from which the latency is measured, and
// sentWall the same instant on the wall clock, from which the window runs. The confirmation window
// runs from acked, the stop's recorded acknowledgedAt, when it has one, for no re-send restarts it;
// otherwise from the first STOPPING's arrival, which onAck records. A window's end is fixed when it
// starts, so a slow read or write inside it does not lengthen it.
func (d *Dispatcher) await(ctx context.Context, robot *fleetv1.Robot, p *pendingEstop, scope metrics.EstopScope,
	epoch uint64, sentFrom, sentWall time.Time, acked *time.Time, onAck func(time.Time)) (Result, error) {
	namespace, robotID, adapter := robot.Namespace, robot.Name, robot.Spec.Adapter.Name
	ch := p.ch

	// First ack (delivery window). The per-adapter ACK wait is namespace-tunable
	// (spec.estop.delivery.perAdapterTimeoutMs, ADR-0016), fail-safe to the built-in
	// default. No ack ⇒ dropped ⇒ Failed, NEVER Stopped.
	deliveryTimeout := d.deliveryTimeoutFor(ctx, namespace)
	deliverBy := d.startWindowAt(p, sentWall.Add(deliveryTimeout))
	var first *fav1.EstopAck
	select {
	case first = <-ch:
	case <-time.After(time.Until(deliverBy)):
		d.settle(p)
		// Sent but no EstopAck: a Command that never acked (metrics-contract result=timeout).
		metrics.IncEstopCommand(namespace, adapter, scope, metrics.ResultTimeout)
		werr := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "no EstopAck within delivery window (dropped estop)", epoch)
		return Result{State: fleetv1.RobotEstopFailed}, joinUnwritten(ErrUndelivered, werr)
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	latency := d.clock().Sub(sentFrom)
	violation := latency > d.slaThreshold
	// RFC-0001 #control-plane-observability-prometheus-metrics-contract: record the round-trip latency for every ack'd estop, and the SLA
	// violation counter alongside the existing EstopLatencyViolation event.
	metrics.ObserveEstopLatency(namespace, adapter, robotID, scope, latency)
	if violation {
		metrics.IncEstopLatencyViolation(namespace, adapter, robotID)
		d.recordLatencyViolation(ctx, robot, latency)
	}
	res := Result{Delivered: true, Latency: latency, LatencyViolation: violation}

	// Confirmed-only STOPPED. The commands_total result reflects this first ack's
	// disposition (RFC-0001 #control-plane-observability-prometheus-metrics-contract); the confirm loop below governs the resulting estopState
	// but is not double-counted.
	switch first.GetState() {
	case fav1.EstopState_ESTOP_STATE_STOPPED:
		metrics.IncEstopCommand(namespace, adapter, scope, metrics.ResultAckStopped)
		res.Confirmed, res.State = true, fleetv1.RobotEstopStopped
		return res, d.recordOutcome(ctx, robot, fleetv1.RobotEstopStopped, "confirmed stopped", epoch)
	case fav1.EstopState_ESTOP_STATE_FAILED:
		metrics.IncEstopCommand(namespace, adapter, scope, metrics.ResultAckFailed)
		res.State = fleetv1.RobotEstopFailed
		return res, d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "adapter reported FAILED: "+first.GetMessage(), epoch)
	default: // STOPPING (or unspecified): stop commanded; await CONFIRMED STOPPED.
		metrics.IncEstopCommand(namespace, adapter, scope, metrics.ResultAckStopping)
		// The window's end is fixed before the Stopping write, which may be slow.
		var confirmBy time.Time
		if acked != nil {
			// A predecessor's record: its time is read on this process's clock.
			confirmBy = d.startWindowAt(p, time.Now().Add(acked.Add(d.confirmTimeout).Sub(d.clock())))
		} else {
			confirmBy = d.startWindowAt(p, time.Now().Add(d.confirmTimeout))
			onAck(d.clock()) // row S2: acknowledgedAt, once
		}
		// Stopping is not an outcome. A failed write of it is logged and the stop goes on:
		// the outcome below is written, or its failure returned, and until then the robot's
		// own trigger or its estopHeldBy record withholds it and pauses its work.
		d.setEstopState(ctx, robot, fleetv1.RobotEstopStopping, "stop commanded; awaiting confirmation", epoch)
		return d.awaitConfirmation(ctx, robot, p, epoch, confirmBy, res)
	}
}

// awaitConfirmation waits, until confirmBy, for the STOPPED that confirms a stop already answered
// STOPPING, and writes the outcome.
func (d *Dispatcher) awaitConfirmation(ctx context.Context, robot *fleetv1.Robot, p *pendingEstop, epoch uint64,
	confirmBy time.Time, res Result) (Result, error) {
	ch := p.ch
	deadline := time.After(time.Until(confirmBy))
	for {
		select {
		case ack := <-ch:
			switch ack.GetState() {
			case fav1.EstopState_ESTOP_STATE_STOPPED:
				res.Confirmed, res.State = true, fleetv1.RobotEstopStopped
				return res, d.recordOutcome(ctx, robot, fleetv1.RobotEstopStopped, "confirmed stopped", epoch)
			case fav1.EstopState_ESTOP_STATE_FAILED:
				res.State = fleetv1.RobotEstopFailed
				return res, d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "adapter reported FAILED", epoch)
			}
			// another STOPPING — keep waiting for a CONFIRMED stop
		case <-deadline:
			d.settle(p)
			// RFC-0001 #safety-estop-state-machine: no STOPPED confirmation within the window ⇒ FAILED. The
			// robot is NEVER assumed stopped.
			res.State = fleetv1.RobotEstopFailed
			return res, d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "no STOPPED confirmation within confirm window", epoch)
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

// register looks up the adapter's live SafetyStream and, when there is one, records the
// estop for the Robot named robot as pending on it under wireID and its stop ID, in one
// step. It returns a nil sender when the adapter has no stream. The second return value of
// the counter is the message's seq, never part of the stop ID.
func (d *Dispatcher) register(namespace, robot, wireID, adapter, estopID string, epoch uint64, estop *fav1.Estop) (
	controlstream.SafetySender, *pendingEstop, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sender := d.streams[adapterKey(namespace, adapter)]
	if sender == nil {
		return nil, nil, 0
	}
	d.idCounter++
	p := &pendingEstop{
		adapter: adapter, sender: sender, ch: make(chan *fav1.EstopAck, 4), robot: robot,
		key:   pendingKey{namespace: namespace, robot: wireID, estopID: estopID},
		estop: estop, epoch: epoch,
	}
	if old := d.pending[p.key]; old != nil && old.taken != nil {
		// A stream-open re-send of the same recorded stop: this waiter takes its answer.
		close(old.taken)
		old.taken = nil
	}
	d.pending[p.key] = p
	return sender, p, d.idCounter
}

// deregister removes the estop's waiter, unless another waiter has taken its stop ID since (a
// takeover and a stream-open re-send of one recorded stop).
func (d *Dispatcher) deregister(p *pendingEstop) {
	d.mu.Lock()
	if d.pending[p.key] == p {
		delete(d.pending, p.key)
	}
	d.mu.Unlock()
}

// startWindow records when the window the waiter is entering ends, and returns it. The
// waiter's timer and a re-send read the same instant, so a re-send never extends it.
func (d *Dispatcher) startWindow(p *pendingEstop, length time.Duration) time.Time {
	end := time.Now().Add(length)
	d.mu.Lock()
	p.deadline = end
	d.mu.Unlock()
	return end
}

// startWindowAt is startWindow for a window whose end, on the wall clock, is fixed by the send or
// the acknowledgement it runs from rather than by when the waiter enters it. The waiter's timer and
// a re-send read the same instant.
func (d *Dispatcher) startWindowAt(p *pendingEstop, end time.Time) time.Time {
	d.mu.Lock()
	p.deadline = end
	d.mu.Unlock()
	return end
}

// settle marks the estop resolved, so no stream takeover re-sends it.
func (d *Dispatcher) settle(p *pendingEstop) {
	d.mu.Lock()
	p.settled = true
	d.mu.Unlock()
}

// settleUnlessResent settles the estop unless it has been re-sent down a stream other
// than from, and reports whether it settled it.
func (d *Dispatcher) settleUnlessResent(p *pendingEstop, from controlstream.SafetySender) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.sender != from {
		return false
	}
	p.settled = true
	return true
}

// estopMessage is the SafetyStream message that carries estop to the wire robot_id
// robotID under estopID.
func estopMessage(robotID string, seq uint64, estop *fav1.Estop, estopID string) *fav1.ControlPlaneSafetyMessage {
	return &fav1.ControlPlaneSafetyMessage{
		Seq:     seq,
		RobotId: robotID,
		Payload: &fav1.ControlPlaneSafetyMessage_Estop{Estop: &fav1.Estop{
			EstopId: estopID, Reason: estop.GetReason(), IssuedBy: estop.GetIssuedBy(), IssuedAtMs: estop.GetIssuedAtMs(),
		}},
	}
}

// resend is one estop to push down the stream that replaced the one it went down.
type resend struct {
	robotID string
	msg     *fav1.ControlPlaneSafetyMessage
}

// rebindLocked moves every estop still pending on the replaced stream onto sender, under
// its own stop ID, and returns the messages to push. The caller holds d.mu.
//
// An estop is moved only while it is still wanted, which is read from state the
// dispatcher already keeps. It is still pending on the replaced stream (not deregistered,
// and not settled by a resolving ack or by the waiter giving up); its window has not
// ended; and no clear and no newer trigger has been issued for the robot since it was
// (the robot's estop ordering record: lastStop is still this trigger's epoch, and
// lastClear is older). A newer trigger is pending in its own right, and is moved on its
// own if it is on the replaced stream.
//
// The estop is bound to sender from here on, so an ack with its stop ID that arrives on the
// replaced stream confirms nothing (RouteAck). The deadline is not touched: the waiter's
// window runs on. The re-send
// carries the robot_id the estop was first sent under, not one read now: the id is
// resolved once per estop.
func (d *Dispatcher) rebindLocked(identity controlstream.TLSIdentity, replaced, sender controlstream.SafetySender) []resend {
	if replaced == nil || replaced == sender {
		return nil
	}
	now := time.Now()
	var moved []*pendingEstop
	for _, p := range d.pending {
		if p.sender != replaced || p.key.namespace != identity.Namespace || p.adapter != identity.AdapterName {
			continue
		}
		if p.settled || (!p.deadline.IsZero() && !now.Before(p.deadline)) {
			continue
		}
		o := d.orders[robotEstopKey(p.key.namespace, p.robot)]
		if o == nil || o.lastStop != p.epoch || o.lastClear > p.epoch {
			continue
		}
		moved = append(moved, p)
	}
	// Map order is random; the pushes go out in a fixed order.
	sort.Slice(moved, func(i, j int) bool { return moved[i].key.estopID < moved[j].key.estopID })
	out := make([]resend, 0, len(moved))
	for _, p := range moved {
		d.idCounter++
		p.sender = sender
		out = append(out, resend{robotID: p.robot, msg: estopMessage(p.key.robot, d.idCounter, p.estop, p.key.estopID)})
	}
	return out
}

// resendOnto pushes each re-sent estop down sender. A push that fails leaves the estop
// waiting, and it resolves Failed when its window ends, as any undelivered estop does.
func (d *Dispatcher) resendOnto(sender controlstream.SafetySender, msgs []resend) {
	logger := log.Log.WithName("estop-dispatcher")
	for _, m := range msgs {
		if err := sender.Send(m.msg); err != nil {
			logger.Error(err, "re-sending an estop down the SafetyStream that replaced the one it went down",
				"robot", m.robotID, "estopID", m.msg.GetEstop().GetEstopId())
			continue
		}
		logger.Info("estop re-sent down the SafetyStream that replaced the one it went down",
			"robot", m.robotID, "estopID", m.msg.GetEstop().GetEstopId())
	}
}

// heldStopIssuer is the issued_by of a stop re-sent because the robot's status holds it.
const heldStopIssuer = "swarmada-control-plane"

// resendHeld sends the stop down sender, a SafetyStream that has just opened for identity's
// adapter, to every robot of that adapter whose status.estopState is Stopping, Stopped or
// Failed, except the robots in moved, whose estop still awaited was just moved onto this
// stream. A Resuming robot's stop was cleared by an operator, and a cleared stop is not
// sent again. It runs once per stream that opens, and each robot is sent its stop once: an
// unanswered re-send is not sent again until another stream opens.
//
// Each stop goes under the ID it was recorded with, one Estop per recorded stop with no
// clearedAt. A robot in a stop state with no such record (an upgraded cluster, or a record lost)
// is sent a stop of its own as row S1 does: a fresh stop ID, recorded as a Robot-scope stop whose
// carrier is the robot, so its next clear names it. A zone's or the namespace's stop in force on it
// with no record is sent by that carrier's next pass, under its own ID.
// Every robot of the adapter is also sent again the EstopClear of each recorded stop that awaits
// its EstopClearAck (resendClears); no Estop and no fence go for those.
//
// The re-send writes no estopState. Its answer is recorded as an Event on the Robot: a
// STOPPED confirms the robot is at rest, and anything else leaves the state the robot
// already holds, which keeps it withheld from dispatch until an operator clears it.
func (d *Dispatcher) resendHeld(identity controlstream.TLSIdentity, sender controlstream.SafetySender, moved map[string]bool) {
	if d.Client == nil {
		return
	}
	logger := log.Log.WithName("estop-dispatcher")
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultConfirmTimeout)
	defer cancel()
	var robots fleetv1.RobotList
	if err := reader.List(ctx, &robots, client.InNamespace(identity.Namespace)); err != nil {
		logger.Error(err, "listing the adapter's robots to re-send the stops they hold down a new SafetyStream",
			"adapter", identity.AdapterName, "namespace", identity.Namespace)
		return
	}
	for i := range robots.Items {
		robot := &robots.Items[i]
		if robot.Spec.Adapter.Name != identity.AdapterName {
			continue
		}
		d.resendClears(robot, sender)
		if moved[robot.Name] {
			continue
		}
		switch robot.Status.EstopState {
		case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed:
			var ids []string
			for _, st := range robot.Status.Stops {
				if st.ClearedAt == nil {
					ids = append(ids, st.ID)
				}
			}
			if len(ids) == 0 {
				id, rec := d.unrecordedStop(robot)
				go d.resendHeldTo(identity, sender, robot.DeepCopy(), id, rec)
			}
			for _, id := range ids {
				go d.resendHeldTo(identity, sender, robot.DeepCopy(), id, nil)
			}
		}
	}
}

// unrecordedStop mints a stop ID for a robot in a stop state that records no stop in force, and
// hands its fence off. resendHeldTo sends it, then records it as a Robot-scope stop whose carrier
// is the robot (the CRD requires every Robot-scope stop to name its carrier). The robot's own
// trigger need not be on it, so the stop may have ended at once: the robot's next clear then
// names it in its EstopClear.
func (d *Dispatcher) unrecordedStop(robot *fleetv1.Robot) (string, *stopRecord) {
	id := NewStopID()
	d.markIssued(id)
	d.sendFence(robot, id, &fav1.Estop{Reason: fmt.Sprintf("held stop re-sent on a new SafetyStream (estopState %s)",
		robot.Status.EstopState), IssuedBy: heldStopIssuer, IssuedAtMs: d.clock().UnixMilli()})
	rec := d.newStopRecord(robot, id, metrics.ScopeRobot, Carrier{}, d.clock())
	rec.finish()
	d.trackInflight(robotEstopKey(robot.Namespace, robot.Name), rec)
	return id, rec
}

// resendHeldTo sends robot's held stop stopID down sender and records what the adapter answers.
// A stop with no record yet (rec) is recorded once it has been sent.
// The stop is awaited under epoch 0, which is no trigger's, so a takeover never moves it
// (rebindLocked): the stream that replaces sender re-sends the stop itself.
func (d *Dispatcher) resendHeldTo(identity controlstream.TLSIdentity, sender controlstream.SafetySender, robot *fleetv1.Robot,
	stopID string, rec *stopRecord) {
	logger := log.Log.WithName("estop-dispatcher").WithValues("robot", robot.Name,
		"namespace", robot.Namespace, "estopState", robot.Status.EstopState)
	wireID := robotid.Wire(robot)
	d.mu.Lock()
	if w := d.pending[pendingKey{namespace: robot.Namespace, robot: wireID, estopID: stopID}]; w != nil {
		// A waiter has the stop already: a trigger, or an earlier pass. On this stream its answer is
		// that waiter's, and nothing is sent twice. On another, the stop is sent again here under its
		// ID and the waiter is bound to this stream, as rebindLocked binds one still wanted: every stop
		// in force goes to a new SafetyStream, and its answer to the one waiter.
		if w.sender == sender {
			d.mu.Unlock()
			logger.Info("held stop not re-sent: it is awaited already on this stream", "estopID", stopID)
			return
		}
		w.sender = sender
		d.idCounter++
		msg := estopMessage(w.key.robot, d.idCounter, w.estop, w.key.estopID)
		d.mu.Unlock()
		if err := sender.Send(msg); err != nil {
			logger.Error(err, "re-sending an awaited stop down a new SafetyStream", "estopID", stopID)
			return
		}
		logger.Info("awaited stop re-sent down a new SafetyStream; its waiter is bound to it", "estopID", stopID)
		return
	}
	d.idCounter++
	taken := make(chan struct{})
	p := &pendingEstop{
		adapter: identity.AdapterName, sender: sender, ch: make(chan *fav1.EstopAck, 4), robot: robot.Name,
		key: pendingKey{namespace: robot.Namespace, robot: wireID, estopID: stopID},
		estop: &fav1.Estop{Reason: fmt.Sprintf("held stop re-sent on a new SafetyStream (estopState %s)",
			robot.Status.EstopState), IssuedBy: heldStopIssuer, IssuedAtMs: d.clock().UnixMilli()},
		taken: taken,
	}
	d.pending[p.key] = p
	seq := d.idCounter
	d.mu.Unlock()
	defer d.deregister(p)

	sentAt := d.clock()
	err := sender.Send(estopMessage(p.key.robot, seq, p.estop, p.key.estopID))
	if rec != nil {
		go d.recordStop(rec)
	}
	if err != nil {
		d.settle(p)
		logger.Error(err, "re-sending a held stop down a new SafetyStream", "estopID", p.key.estopID)
		d.recordHeldResend(robot, false, "the send failed: "+err.Error(), sentAt, Result{State: fleetv1.RobotEstopFailed})
		return
	}
	logger.Info("held stop re-sent down a new SafetyStream", "estopID", p.key.estopID)

	// The re-send is an estop Command like a first send, and is measured, metered and
	// audited as one (RFC-0001 #safety-timing-requirements, the ESTOP_TRIGGERED row): the
	// first acknowledgement's latency and disposition, or a timeout when none comes.
	ctx := context.Background()
	adapter, scope := robot.Spec.Adapter.Name, resendScope(robot)
	var res Result
	window := time.After(time.Until(d.startWindow(p,
		d.deliveryTimeoutFor(ctx, robot.Namespace)+d.confirmTimeout)))
	for {
		select {
		case <-taken:
			logger.Info("held stop's re-send handed to the trigger that runs it out", "estopID", p.key.estopID)
			return
		case ack := <-p.ch:
			if !d.stillLeads(robot, "") {
				return
			}
			if !res.Delivered {
				res.Delivered, res.Latency = true, d.clock().Sub(sentAt)
				res.LatencyViolation = res.Latency > d.slaThreshold
				metrics.ObserveEstopLatency(robot.Namespace, adapter, robot.Name, scope, res.Latency)
				if res.LatencyViolation {
					metrics.IncEstopLatencyViolation(robot.Namespace, adapter, robot.Name)
					d.recordLatencyViolation(ctx, robot, res.Latency)
				}
				metrics.IncEstopCommand(robot.Namespace, adapter, scope, ackResult(ack.GetState()))
			}
			switch ack.GetState() {
			case fav1.EstopState_ESTOP_STATE_STOPPED:
				res.Confirmed, res.State = true, fleetv1.RobotEstopStopped
				d.recordHeldResend(robot, true, "the adapter confirmed STOPPED", sentAt, res)
				return
			case fav1.EstopState_ESTOP_STATE_FAILED:
				res.State = fleetv1.RobotEstopFailed
				d.recordHeldResend(robot, false, "the adapter answered FAILED: "+ack.GetMessage(), sentAt, res)
				return
			}
		case <-window:
			d.settle(p)
			if !d.stillLeads(robot, "") {
				return
			}
			if !res.Delivered {
				metrics.IncEstopCommand(robot.Namespace, adapter, scope, metrics.ResultTimeout)
			}
			res.State = fleetv1.RobotEstopFailed
			d.recordHeldResend(robot, false, "the adapter did not confirm STOPPED within the window", sentAt, res)
			return
		}
	}
}

// ackResult is the commands_total result of a first acknowledgement.
func ackResult(state fav1.EstopState) string {
	switch state {
	case fav1.EstopState_ESTOP_STATE_STOPPED:
		return metrics.ResultAckStopped
	case fav1.EstopState_ESTOP_STATE_FAILED:
		return metrics.ResultAckFailed
	}
	return metrics.ResultAckStopping
}

// resendScope is the scope label of a held stop sent again: the robot's own trigger when it
// carries one, else the widest-named recorded hold (a zone's, then the namespace's).
func resendScope(robot *fleetv1.Robot) metrics.EstopScope {
	if _, own := robot.Annotations[estopscope.TriggerAnnotation]; own {
		return metrics.ScopeRobot
	}
	scope := metrics.ScopeRobot
	for _, h := range robot.Status.EstopHeldBy {
		switch h.Kind {
		case fleetv1.EstopHoldFleetZone:
			return metrics.ScopeZone
		case fleetv1.EstopHoldSwarmadaConfig:
			scope = metrics.ScopeNamespace
		}
	}
	return scope
}

// recordHeldResend records the answer to a held stop re-sent down a new SafetyStream as an
// Event on the Robot and an ESTOP_TRIGGERED audit entry carrying the robot-scope timing
// fields. It writes no state: the stop the robot holds stands either way.
func (d *Dispatcher) recordHeldResend(robot *fleetv1.Robot, confirmed bool, answer string, sentAt time.Time, res Result) {
	d.auditHeldResend(robot, confirmed, answer, sentAt, res)
	if d.Recorder == nil {
		return
	}
	if confirmed {
		d.Recorder.Eventf(robot, corev1.EventTypeNormal, "EstopResent",
			"the stop this robot holds (estopState %s) was sent again on its adapter's new SafetyStream; %s",
			robot.Status.EstopState, answer)
		return
	}
	d.Recorder.Eventf(robot, corev1.EventTypeWarning, "EstopResendUnconfirmed",
		"the stop this robot holds (estopState %s) was sent again on its adapter's new SafetyStream; %s. "+
			"estopState stays %s and the robot stays withheld", robot.Status.EstopState, answer, robot.Status.EstopState)
}

// auditHeldResend seals ESTOP_TRIGGERED for a held stop sent again, as the robot-scope entry
// of a first send is sealed: reason, state and the timing fields; outcome Error when the
// re-send was not confirmed STOPPED. Best-effort, like every other audit producer here.
func (d *Dispatcher) auditHeldResend(robot *fleetv1.Robot, confirmed bool, answer string, sentAt time.Time, res Result) {
	if d.Audit == nil {
		return
	}
	outcome := audit.OutcomeAllowed
	if !confirmed {
		outcome = audit.OutcomeError
	}
	detail := map[string]string{
		"reason":  "held stop re-sent on a new SafetyStream (estopState " + string(robot.Status.EstopState) + "): " + answer,
		"state":   string(res.State),
		"resent":  "true",
		"adapter": robot.Spec.Adapter.Name,
	}
	detail["rpc_sent_at"] = sentAt.UTC().Format(time.RFC3339Nano)
	detail["delivered"] = strconv.FormatBool(res.Delivered)
	if res.Delivered {
		detail["ack_received_at"] = sentAt.Add(res.Latency).UTC().Format(time.RFC3339Nano)
		detail["ack_latency_ms"] = strconv.FormatInt(res.Latency.Milliseconds(), 10)
		detail["latency_violation"] = strconv.FormatBool(res.LatencyViolation)
	}
	if _, err := d.Audit.Record(audit.Entry{
		EventType: audit.EventEstopTriggered,
		Namespace: robot.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "safety-dispatcher"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    "estop",
		Outcome:   outcome,
		Detail:    detail,
	}); err != nil {
		log.Log.WithName("estop-dispatcher").Error(err, "recording ESTOP_TRIGGERED for a re-sent held stop", "robot", robot.Name)
	}
}

// ConfirmResume is the operator's release of a robot from the post-clear supervised
// window, and the ONLY path from Resuming to Normal. The robot-estop controller calls
// it once per operator request, and at no other time. There is no automatic release
// from Resuming — not on telemetry, not on adapter presence, not on phase (RFC-0001
// #safety-estop-state-machine) — so a robot nobody confirms stays Resuming, withheld
// from dispatch, indefinitely.
//
// The confirmation is REFUSED, and the reason returned, while the robot itself does
// not look ready. Against the robot as it stands now, it checks:
//
//   - that the robot does not carry its own robot-scope trigger, which holds it until
//     that annotation is removed, whatever cleared a wider stop on it, and that no zone
//     or namespace stop is recorded on it (status.estopHeldBy), which holds it until
//     that stop's own clear;
//   - that the robot has sent telemetry since the clear (on this control-plane leader),
//     so the checks below read post-clear evidence and not the projection from before
//     the stop; for a robot this leader found in Resuming, telemetry strictly later than
//     the first frame this leader received from it;
//   - that the robot's most recent report has reached Robot.status: its status write
//     did not fail, and it is not held by the projection rate cap. The checks below read
//     status, so while the robot's latest report is not in it they would judge an older
//     one;
//   - that the robot's own most recent report does not put it in Error or Offline;
//   - that no hardware component is reported Failed;
//   - that every capability that was Active when the stop was taken
//     (status.estopBaseline) is Active again, both as derived from the reported
//     hardware now and as stored on the Robot, which is what the Scheduler reads.
//
// These checks guard the operator's decision; they do not replace it. They read the
// control plane's view of the robot, which is the robot's own reports as written to
// status, so a confirmation that passes them is still an operator's judgement that the
// robot is ready. That is why the release is operator-confirmed and not automatic.
//
// It returns the state that stands and, when the robot does not reach Normal, why. It
// is a no-op, and says so, for a robot that is not Resuming.
//
// The write is licensed under RA-1 as a material transition: at most one per operator
// request, on the edge, never at telemetry cadence.
//
// Single writer. RFC-0001 #safety-estop-state-machine requires that
// Robot.status.estopState have exactly one writer — "the estop dispatcher on the
// elected control-plane leader" — which is what makes the per-robot epoch total. The
// controller that reads the operator's request calls in; the dispatcher writes.
func (d *Dispatcher) ConfirmResume(ctx context.Context, namespace, robotID, confirmedBy string) (fleetv1.RobotEstopState, string, error) {
	// A robot that carries its own robot-scope trigger is held by it until that annotation
	// is removed, whatever moved it to Resuming: a zone- or namespace-scope clear is not
	// its clear (RFC-0001 #safety-estop-scopes). This comes first, so no other answer
	// stands in for it.
	if state, why, held, err := d.heldByOwnTrigger(ctx, namespace, robotID); err != nil || held {
		return state, why, err
	}
	key := robotEstopKey(namespace, robotID)
	if d.existingOrder(key) == nil {
		// No ordering record: either this leader never estopped this robot, or
		// leadership changed since the clear and the in-memory baseline went with it.
		// Only the second can be Resuming, and it is re-armed rather than confirmed:
		// the operator confirms again once the robot has reported to this leader.
		return d.rearmResumeAfterFailover(ctx, namespace, robotID, key)
	}
	// Taken BEFORE the read, for the same reason ClearEstop takes one: a trigger
	// issued while this confirmation is in flight must be ordered after it, so the
	// release below loses to it.
	epoch := d.issueEstopEpoch(key, false)

	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	robot := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return "", "", fmt.Errorf("estop-resume: robot %q lookup: %w", robotID, err)
	}
	if robot.Status.EstopState != fleetv1.RobotEstopResuming {
		return robot.Status.EstopState, notResuming(robot.Status.EstopState), nil
	}

	baseline, armed, adopted := d.resumeBaseline(key)
	if !armed {
		// This leader holds a record for the robot but no baseline, so it did not accept the
		// robot's clear: a clear it wrote arms the window. A clear of its own that is writing
		// Resuming now arms it after this, and its baseline replaces this one.
		d.adoptResumeWindow(key)
		return fleetv1.RobotEstopResuming,
			"no post-clear baseline on this control-plane leader; confirm again after the robot has reported", nil
	}
	if adopted {
		first, seen := d.firstFrame(robot)
		if !seen {
			return fleetv1.RobotEstopResuming,
				"the robot has sent no telemetry since this control-plane leader took over", nil
		}
		baseline = first
	}
	heard, ok := d.lastFrame(robot)
	if !ok || !heard.After(baseline) {
		return fleetv1.RobotEstopResuming, "the robot has sent no telemetry since the clear", nil
	}
	if d.lastReportUnwritten(robot) {
		return fleetv1.RobotEstopResuming,
			"the robot's most recent report has not reached Robot.status yet (its status write failed or is " +
				"held by the projection rate cap), so the checks would read an older report; confirm again " +
				"after the robot's next report is written", nil
	}
	if d.DeriveCapabilities == nil {
		return fleetv1.RobotEstopResuming, "capability derivation is not configured on this control plane", nil
	}
	why, ok := resumeRefusal(robot, d.DeriveCapabilities(robot), d.reportedPhase(robot))
	if !ok {
		log.FromContext(ctx).Info("estop resume refused; robot stays in the post-clear window",
			"robot", robot.Name, "namespace", robot.Namespace, "confirmedBy", confirmedBy, "reason", why)
		return fleetv1.RobotEstopResuming, why, nil
	}
	// The release releases the robot, not its work: the work the clear interrupted is held before
	// Normal is written, whether or not the clear's process held it (holdClearedWork).
	if err := d.holdClearedWork(ctx, robot); err != nil {
		return fleetv1.RobotEstopResuming, "", fmt.Errorf("estop-resume: robot %q: the work it records is not held yet: %w", robotID, err)
	}
	written, werr := d.writeEstopState(ctx, robot, fleetv1.RobotEstopNormal, "resume confirmed by "+confirmedBy+": "+why, epoch)
	if werr != nil {
		return robot.Status.EstopState, "", fmt.Errorf("estop-resume: robot %q: %w", robotID, werr)
	}
	if !written {
		return robot.Status.EstopState, "a later estop event superseded this confirmation", nil
	}
	d.disarmResumeWindow(key)
	// What a stop withheld from the robot (a zone admission, a reservation grant) goes now, each
	// through the stop check again (RFC-0001 #safety-how-the-two-interact, I6).
	if d.Fence != nil {
		d.Fence.RobotReleased(namespace, robotID)
	}
	log.FromContext(ctx).Info("estop resume confirmed by an operator; robot returned to Normal",
		"robot", robot.Name, "namespace", robot.Namespace, "confirmedBy", confirmedBy, "evidence", why)
	return fleetv1.RobotEstopNormal, why, nil
}

// heldByOwnTrigger reads the robot from the API server and reports whether a stop it
// records holds it: its own robot-scope trigger, whose presence counts whatever its value,
// or a zone's or the namespace's stop recorded in status.estopHeldBy (estopscope.HeldOnRecord).
// It returns the state that stands and why the robot is not released.
func (d *Dispatcher) heldByOwnTrigger(ctx context.Context, namespace, robotID string) (fleetv1.RobotEstopState, string, bool, error) {
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	robot := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return "", "", false, fmt.Errorf("estop-resume: robot %q lookup: %w", robotID, err)
	}
	by, held := estopscope.HeldOnRecord(robot)
	if !held {
		return "", "", false, nil
	}
	return robot.Status.EstopState, "it is still held by " + by.String(), true, nil
}

// notResuming is the answer to a confirmation for a robot outside the window.
func notResuming(state fleetv1.RobotEstopState) string {
	if state == "" {
		state = fleetv1.RobotEstopNormal
	}
	return fmt.Sprintf("the robot is not in Resuming (estopState %s); there is nothing to confirm", state)
}

// rearmResumeAfterFailover adopts a robot found Resuming with no in-memory ordering
// record: its baseline is the first frame this leader receives from it (RFC-0001
// #safety-estop-state-machine). It never releases the stop.
func (d *Dispatcher) rearmResumeAfterFailover(ctx context.Context, namespace, robotID, key string) (fleetv1.RobotEstopState, string, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return "", "", fmt.Errorf("estop-resume: robot %q lookup: %w", robotID, err)
	}
	if robot.Status.EstopState != fleetv1.RobotEstopResuming {
		return robot.Status.EstopState, notResuming(robot.Status.EstopState), nil
	}
	d.adoptResumeWindow(key)
	log.FromContext(ctx).Info("adopted a robot already in the post-clear estop window; awaiting fresh telemetry",
		"robot", robot.Name, "namespace", robot.Namespace)
	return fleetv1.RobotEstopResuming,
		"this control-plane leader holds no record of the clear; confirm again after the robot has reported", nil
}

// resumeRefusal reports whether an operator's confirmation may release the robot, and
// returns either the evidence or the specific thing that refuses it. Freshness is the
// caller's check; this is the condition itself.
//
// It does NOT require Robot.status.phase to be Idle or InProgress. That field is
// derived by the control plane (from adapter liveness, assignment and maintenance),
// never from the robot's own report, so it is not evidence about the robot; and a
// robot paused by a maintenance window sits in Maintenance for as long as the window
// is open, while the window's close waits for the robot's estop to be Normal — a phase
// requirement here would leave both waiting for ever. The robot's own last-reported
// phase is read instead (reported), when the robot reports one. Of the control
// plane's own phases only Error and Offline refuse: each records that something is
// wrong with the robot, where Maintenance, Charging or Assigned do not.
//
// Every capability that was Active when the stop was taken must be Active again, not
// merely the ones some action happens to require — that is what makes this stronger
// than Scheduler candidate filter 3 (RFC-0001 #control-plane-scheduler). A capability
// that was not Active before the stop (Inactive because the robot never reported its
// hardware, or Paused by a maintenance window) does not gate the release: the stop did
// not take it away, so the release cannot wait for it to come back, and requiring it
// left the robot Resuming for ever. A capability Paused by a maintenance window does
// not gate it either, whenever the window opened: the window's close waits for this
// release, so the capability cannot be Active until the release happens. Without a
// record of the stop (status.estopBaseline absent: the stop predates it), every
// capability must be Active. Capabilities are checked twice: derived is the list
// computed from the hardware the robot reported, and the stored list is what the
// Scheduler reads the moment the robot is Normal.
func resumeRefusal(robot *fleetv1.Robot, derived []fleetv1.CapabilityStatusEntry, reported fleetv1.RobotPhase) (string, bool) {
	switch reported {
	case fleetv1.RobotPhaseError, fleetv1.RobotPhaseOffline:
		return fmt.Sprintf("the robot's most recent report puts it in %s", reported), false
	}
	switch robot.Status.Phase {
	case fleetv1.RobotPhaseError, fleetv1.RobotPhaseOffline:
		return fmt.Sprintf("the control plane has marked the robot %s", robot.Status.Phase), false
	}
	for _, h := range robot.Status.Hardware {
		if h.Status == fleetv1.HardwareFailed {
			return fmt.Sprintf("hardware component %q is reported Failed; %s", h.Name,
				resumeRemedy(robot, "the robot reports the component repaired", "component", "spec.hardware")), false
		}
	}
	for _, c := range derived {
		if why, refused := capabilityRefusal(robot, c, " on the reported hardware"); refused {
			return why, false
		}
	}
	for _, c := range robot.Status.Capabilities {
		if why, refused := capabilityRefusal(robot, c, ""); refused {
			return why, false
		}
	}
	return resumeEvidence(robot), true
}

// resumeEvidence is what a release records as checked: the EstopResumeConfirmed Event,
// the ESTOP_RESUME_CONFIRMED audit entry, and what the resume command prints. It counts
// the stored list, which is what the Scheduler reads once the robot is Normal. A
// capability Paused by a maintenance window, or one recorded at the stop and no longer
// declared, did not hold the release, so it is named as not checked rather than counted
// as Active.
func resumeEvidence(robot *fleetv1.Robot) string {
	b := robot.Status.EstopBaseline
	if b == nil {
		active, paused := 0, 0
		for _, c := range robot.Status.Capabilities {
			if c.Paused {
				paused++
			} else {
				active++
			}
		}
		s := fmt.Sprintf("%d %s Active", active, plural(active, "capability", "capabilities"))
		if paused > 0 {
			s += fmt.Sprintf(" and %d Paused by a maintenance window (not checked)", paused)
		}
		return s + "; no hardware Failed"
	}
	active, paused, undeclared := 0, 0, 0
	for _, name := range b.ActiveCapabilities {
		i := slices.IndexFunc(robot.Status.Capabilities, func(c fleetv1.CapabilityStatusEntry) bool { return c.Name == name })
		switch {
		case i < 0:
			undeclared++
		case robot.Status.Capabilities[i].Paused:
			paused++
		default:
			active++
		}
	}
	parts := []string{fmt.Sprintf("%d %s Active again", active, plural(active, "is", "are"))}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d %s Paused by a maintenance window (not checked)", paused, plural(paused, "is", "are")))
	}
	if undeclared > 0 {
		parts = append(parts, fmt.Sprintf("%d %s no longer declared (not checked)", undeclared, plural(undeclared, "is", "are")))
	}
	checked := parts[len(parts)-1]
	if len(parts) > 1 {
		checked = strings.Join(parts[:len(parts)-1], ", ") + " and " + checked
	}
	return fmt.Sprintf("of the %d %s Active at the stop, %s; no hardware Failed",
		len(b.ActiveCapabilities), plural(len(b.ActiveCapabilities), "capability", "capabilities"), checked)
}

// plural picks the singular form for exactly one.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// capabilityRefusal reports whether capability c refuses the release, and why. where
// says which list c came from.
func capabilityRefusal(robot *fleetv1.Robot, c fleetv1.CapabilityStatusEntry, where string) (string, bool) {
	if c.Status == fleetv1.CapabilityStatusActive || c.Paused {
		return "", false
	}
	remedy := resumeRemedy(robot, "its hardware is repaired", "capability", "spec.baseCapabilities")
	b := robot.Status.EstopBaseline
	if b == nil {
		return fmt.Sprintf("capability %q is %s%s, not Active, and this stop has no record of which capabilities "+
			"were Active when it was taken, so every capability must be Active; %s", c.Name, c.Status, where, remedy), true
	}
	if !slices.Contains(b.ActiveCapabilities, c.Name) {
		return "", false
	}
	return fmt.Sprintf("capability %q was Active when the stop was taken and is %s%s now; %s",
		c.Name, c.Status, where, remedy), true
}

// activeCapabilities names the capabilities Active on the stored list or on derived,
// sorted, for the record a stop takes.
func activeCapabilities(stored, derived []fleetv1.CapabilityStatusEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]fleetv1.CapabilityStatusEntry{stored, derived} {
		for _, c := range list {
			if c.Status == fleetv1.CapabilityStatusActive && !seen[c.Name] {
				seen[c.Name] = true
				out = append(out, c.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resumeRemedy tells the operator what ends a refusal that has no override. A robot
// with a Failed component or a capability that is not Active stays Resuming until the
// fault is repaired, or until the item is removed from the robot's declaration: from
// its RobotClass, then re-applied to the robot with `swarmctl robotclass rollout`,
// which replaces the robot's hardware and capability lists with the class's.
func resumeRemedy(robot *fleetv1.Robot, repaired, item, field string) string {
	class := robot.Spec.RobotClass
	if class == "" {
		return fmt.Sprintf("the robot stays Resuming until %s, or the %s is removed from the Robot's %s",
			repaired, item, strings.TrimPrefix(field, "spec.base"))
	}
	return fmt.Sprintf("the robot stays Resuming until %s, or the %s is removed from RobotClass %q (%s) "+
		"and applied with `swarmctl robotclass rollout %s`", repaired, item, class, field, class)
}

// TelemetryClock reports what the robot's own telemetry last said. It holds no API
// state, so reading it costs no etcd write (RA-1).
// [github.com/swarmada/swarmada/internal/telemetry.Router] satisfies it.
type TelemetryClock interface {
	// LastFrame is when the robot's most recent frame was received, keyed by the
	// robot's namespace and wire robot_id; ok is false when none has been received.
	LastFrame(namespace, robotID string) (at time.Time, ok bool)
	// FirstFrame is when the first frame this process received from the robot arrived,
	// keyed as LastFrame is; ok is false when none has been received.
	FirstFrame(namespace, robotID string) (at time.Time, ok bool)
	// LastReportedPhase is the phase in the robot's most recent frame that carried
	// one; ok is false when none has.
	LastReportedPhase(namespace, robotID string) (phase fleetv1.RobotPhase, ok bool)
	// LastReportUnwritten reports whether the robot's most recent frame carried a
	// change that has not reached Robot.status: its status write failed, or the
	// projection rate cap is holding it.
	LastReportUnwritten(namespace, robotID string) bool
}

// lastFrame is when the robot's own telemetry was last received. A robot with no wire
// id, or a dispatcher with no telemetry, has no frame to show.
func (d *Dispatcher) lastFrame(robot *fleetv1.Robot) (time.Time, bool) {
	id := robot.Annotations[fleetv1.RobotIDAnnotation]
	if d.Telemetry == nil || id == "" {
		return time.Time{}, false
	}
	return d.Telemetry.LastFrame(robot.Namespace, id)
}

// firstFrame is when the first frame this process received from the robot arrived. A robot
// with no wire id, or a dispatcher with no telemetry, has no frame to show.
func (d *Dispatcher) firstFrame(robot *fleetv1.Robot) (time.Time, bool) {
	id := robot.Annotations[fleetv1.RobotIDAnnotation]
	if d.Telemetry == nil || id == "" {
		return time.Time{}, false
	}
	return d.Telemetry.FirstFrame(robot.Namespace, id)
}

// reportedPhase is the robot's own last-reported phase, or "" when it has reported none.
func (d *Dispatcher) reportedPhase(robot *fleetv1.Robot) fleetv1.RobotPhase {
	id := robot.Annotations[fleetv1.RobotIDAnnotation]
	if d.Telemetry == nil || id == "" {
		return ""
	}
	p, _ := d.Telemetry.LastReportedPhase(robot.Namespace, id)
	return p
}

// lastReportUnwritten reports whether the robot's most recent report has not reached
// Robot.status. A robot with no wire id, or a dispatcher with no telemetry, has no
// report to wait for; the freshness check has already refused it.
func (d *Dispatcher) lastReportUnwritten(robot *fleetv1.Robot) bool {
	id := robot.Annotations[fleetv1.RobotIDAnnotation]
	if d.Telemetry == nil || id == "" {
		return false
	}
	return d.Telemetry.LastReportUnwritten(robot.Namespace, id)
}

// armResumeWindow records the freshness baseline for a robot entering Resuming.
func (d *Dispatcher) armResumeWindow(key string, baseline time.Time) {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	o.resumeArmed, o.resumeSince, o.resumeAdopted = true, baseline, false
}

// adoptResumeWindow arms the window of a robot this leader found Resuming and whose clear
// it did not accept, with the first frame it receives as the baseline. A window armed
// already is left as it is: it was armed by this leader's own clear.
func (d *Dispatcher) adoptResumeWindow(key string) {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	if o.resumeArmed {
		return
	}
	o.resumeArmed, o.resumeSince, o.resumeAdopted = true, time.Time{}, true
}

// disarmResumeWindow forgets the baseline once the window has been left.
func (d *Dispatcher) disarmResumeWindow(key string) {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	o.resumeArmed, o.resumeSince = false, time.Time{}
}

// resumeBaseline returns the robot's resume freshness baseline, whether one is
// established, and whether the robot was adopted (its baseline is then its first frame,
// not the time returned). An unestablished baseline never satisfies the exit.
func (d *Dispatcher) resumeBaseline(key string) (time.Time, bool, bool) {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	return o.resumeSince, o.resumeArmed, o.resumeAdopted
}

// existingOrder returns this robot's ordering record WITHOUT creating one, so a
// read-only probe cannot grow the map to fleet size.
func (d *Dispatcher) existingOrder(key string) *estopOrder {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.orders[key]
}

// orderFor returns this robot's estop ordering record, creating it on first use.
func (d *Dispatcher) orderFor(key string) *estopOrder {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.orders == nil {
		d.orders = map[string]*estopOrder{}
	}
	o := d.orders[key]
	if o == nil {
		o = &estopOrder{}
		d.orders[key] = o
	}
	return o
}

// issueEstopEpoch allocates this robot's next estop epoch and returns it. stop
// reports whether the event is a trigger; a trigger also advances lastStop, which
// is what a later-arriving clear is checked against.
//
// It is taken at the moment the event is ISSUED — before any read or send that
// could block — because "which happened later" has to mean the operator action,
// not the API round trip.
func (d *Dispatcher) issueEstopEpoch(key string, stop bool) uint64 {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	o.issued++
	if stop {
		o.lastStop = o.issued
	}
	return o.issued
}

// issueClearEpoch is issueEstopEpoch for a clear, which it also records as the robot's
// last clear: a trigger issued before it is no longer wanted and is not re-sent.
func (d *Dispatcher) issueClearEpoch(key string) uint64 {
	o := d.orderFor(key)
	d.mu.Lock()
	defer d.mu.Unlock()
	o.issued++
	o.lastClear = o.issued
	return o.issued
}

// isEstopRelease reports whether a state write moves a robot OUT of a stop, and so
// must lose to a trigger issued after the releasing event. Resuming and Normal are
// the two halves of the release path (RFC-0001 #safety-estop-state-machine,
// Stopped → Resuming → Normal); everything else is a stop and is never gated.
func isEstopRelease(state fleetv1.RobotEstopState) bool {
	return state == fleetv1.RobotEstopNormal || state == fleetv1.RobotEstopResuming
}

// supersededByLaterStop reports whether a trigger was issued for this robot AFTER
// the event carrying epoch.
func (d *Dispatcher) supersededByLaterStop(o *estopOrder, epoch uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return o.lastStop > epoch
}

// setEstopState writes a confirmed estop transition to Robot.status.estopState.
// It is a material safety transition (never a telemetry tick — RA-1) and a no-op
// when the stored state is already the one being written. It reports whether the
// transition was written.
//
// "Stored" means re-read here, not the caller's copy. robot was read when its trigger
// started, and another trigger's outcome may have been written since; comparing
// against that copy dropped a newer confirmed Stopped over an older trigger's Failed.
//
// epoch is the estop epoch of the event this write belongs to. The two directions
// are treated differently, and the asymmetry is the whole point:
//
// - A stop (Stopping/Stopped/Failed) is written UNCONDITIONALLY, with one exception
// that refuses no stop: an outcome of a trigger OLDER than the one whose confirmed
// Stopped stands is not written, and a Failed among them is recorded as a Warning
// Event instead. The robot is confirmed at rest, and the newest outcome is the truth,
// so a NEWER trigger's Failed still overwrites that Stopped. Estop carries no fence,
// so a stale epoch must never be able to refuse or delay a stop.
// - A release (Resuming or Normal) is written only when no trigger has been issued
// for this robot since the releasing event was issued. Without that check a clear
// that merely reached the API server last could override a live estop and return
// the robot to the scheduler's candidate set (internal/controller/adapter_readiness.go).
//
// BOTH release states are gated, not just Normal. Resuming is the first half of the
// release, and letting it through ungated would let a superseded clear overwrite a
// CONFIRMED Stopped — which RFC-0001 #safety-estop-state-machine forbids outright:
// "a confirmed stop is never discarded". It would also silently lift the robot out
// of candidate filter 10, which excludes Stopped and does not exclude Resuming.
//
// The check and the patch are serialised per robot so a trigger cannot register
// between them.
//
// The same patch carries status.estopBaseline on the two transitions that own it: the
// first stop of an episode (a stop state written over Normal) records the capabilities
// Active at that moment, and Normal removes the record. A stop written inside an
// episode (over Stopping, Stopped, Failed or Resuming) keeps the record the episode
// began with, so a capability lost during the stop still gates the release; an episode
// that began with no record stays without one.
func (d *Dispatcher) setEstopState(ctx context.Context, robot *fleetv1.Robot, state fleetv1.RobotEstopState, reason string, epoch uint64) bool {
	if !d.stillLeads(robot, state) {
		return false
	}
	written, _ := d.writeEstopState(ctx, robot, state, reason, epoch)
	return written
}

// joinUnwritten adds a failed outcome write to a delivery error, and returns the delivery
// error itself when the outcome was written, so a caller comparing it with ErrUndelivered
// still can.
func joinUnwritten(deliveryErr, writeErr error) error {
	if writeErr == nil {
		return deliveryErr
	}
	return errors.Join(deliveryErr, writeErr)
}

// recordOutcome writes a stop's outcome and returns ErrStateUnwritten, wrapped, when the
// write fails. A write the ordering rules skip (an older trigger's outcome under a newer
// confirmed Stopped, or a state already stored) is not a failure, and neither is an outcome a
// process that no longer leads does not write.
func (d *Dispatcher) recordOutcome(ctx context.Context, robot *fleetv1.Robot, state fleetv1.RobotEstopState, reason string, epoch uint64) error {
	if !d.stillLeads(robot, state) {
		return nil
	}
	if _, err := d.writeEstopState(ctx, robot, state, reason, epoch); err != nil {
		return fmt.Errorf("%w (%s): %v", ErrStateUnwritten, state, err)
	}
	return nil
}

// writeEstopState is setEstopState that also returns the patch error, when the write was
// attempted and failed.
func (d *Dispatcher) writeEstopState(ctx context.Context, robot *fleetv1.Robot, state fleetv1.RobotEstopState, reason string, epoch uint64) (bool, error) {
	o := d.orderFor(robotEstopKey(robot.Namespace, robot.Name))
	o.write.Lock()
	defer o.write.Unlock()

	if isEstopRelease(state) && d.supersededByLaterStop(o, epoch) {
		log.FromContext(ctx).Info("estop clear superseded by a trigger issued after it; the stop stands",
			"robot", robot.Name, "epoch", epoch, "reason", reason)
		return false, nil
	}
	if !isEstopRelease(state) && o.stoppedBy > epoch {
		log.FromContext(ctx).Info("estop outcome of an older trigger; a newer trigger's confirmed stop stands",
			"robot", robot.Name, "epoch", epoch, "stoppedBy", o.stoppedBy, "state", state, "reason", reason)
		if state == fleetv1.RobotEstopFailed {
			d.recordSupersededOutcome(robot, reason)
		}
		return false, nil
	}
	// Every write carries a resourceVersion precondition (Rules for every S row), so another write
	// to the Robot between the read and the patch is a conflict. A release (Resuming, Normal) reads
	// again and checks its guard again, five times at most. A stop's outcome is never dropped for a
	// conflict: it is retried with a backoff until it lands, this process stops leading, or the
	// context ends.
	backoff := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		written, err := d.writeEstopStateOnce(ctx, o, robot, state, reason, epoch)
		if err == nil || !apierrors.IsConflict(err) {
			return written, err
		}
		if isEstopRelease(state) && attempt == 4 {
			return written, err
		}
		if !isEstopRelease(state) {
			// Thirty conflicts in a row (about half a minute) means the read itself fails; the
			// error is returned as ErrStateUnwritten, and the caller issues the stop again.
			if _, leading := d.leads(); !leading || ctx.Err() != nil || attempt == 29 {
				return written, err
			}
		}
		o.write.Unlock()
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		o.write.Lock()
		backoff = min(2*backoff, time.Second)
	}
}

// writeEstopStateOnce is one read and patch of writeEstopState, under the robot's write lock.
func (d *Dispatcher) writeEstopStateOnce(ctx context.Context, o *estopOrder, robot *fleetv1.Robot,
	state fleetv1.RobotEstopState, reason string, epoch uint64) (bool, error) {
	stored := d.storedRobot(ctx, robot)
	// Normal completes a clear: it is written only over Resuming. A stop confirmed after
	// the resume confirmation read Resuming, by a trigger issued before that confirmation
	// (so the epoch check above lets it through), stands: a confirmed stop is never
	// discarded (RFC-0001 #safety-estop-state-machine), and a confirmation never releases one.
	if state == fleetv1.RobotEstopNormal && stored.Status.EstopState != fleetv1.RobotEstopResuming {
		log.FromContext(ctx).Info("estop resume not written: the robot left Resuming before the write; the stop stands",
			"robot", robot.Name, "epoch", epoch, "stored", stored.Status.EstopState, "reason", reason)
		robot.Status.EstopState = stored.Status.EstopState
		return false, nil
	}
	// S6 releases the robot only if no stop is in force on it, read again at the write: on the
	// stored Robot (its own trigger, a recorded zone or namespace stop, a recorded stop with no
	// clearedAt) and in this process's own state (a stop sent and not recorded yet, a stop in
	// force for its AssignActions).
	if state == fleetv1.RobotEstopNormal {
		if why, held := d.stopInForce(stored); held {
			log.FromContext(ctx).Info("estop resume not written: a stop is in force on the robot; it stays Resuming",
				"robot", robot.Name, "epoch", epoch, "stop", why)
			robot.Status.EstopState = stored.Status.EstopState
			return false, nil
		}
	}
	if stored.Status.EstopState == state {
		robot.Status.EstopState = state
		if state == fleetv1.RobotEstopStopped {
			o.stoppedBy = epoch
		}
		return false, nil
	}
	base := stored.DeepCopy()
	stored.Status.EstopState = state
	switch {
	case state == fleetv1.RobotEstopNormal:
		stored.Status.EstopBaseline = nil
	case !isEstopRelease(state) && base.Status.EstopBaseline == nil &&
		(base.Status.EstopState == "" || base.Status.EstopState == fleetv1.RobotEstopNormal):
		var derived []fleetv1.CapabilityStatusEntry
		if d.DeriveCapabilities != nil {
			derived = d.DeriveCapabilities(stored)
		}
		stored.Status.EstopBaseline = &fleetv1.EstopBaseline{
			ActiveCapabilities: activeCapabilities(stored.Status.Capabilities, derived),
			RecordedAt:         metav1.NewTime(d.clock()),
		}
	}
	if err := d.Client.Status().Patch(ctx, stored, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if !apierrors.IsConflict(err) {
			log.FromContext(ctx).Error(err, "writing estopState", "robot", robot.Name, "state", state, "reason", reason)
		}
		return false, err
	}
	robot.Status.EstopState = state
	robot.Status.EstopBaseline = stored.Status.EstopBaseline
	o.stoppedBy = 0
	if state == fleetv1.RobotEstopStopped {
		o.stoppedBy = epoch
	}
	return true, nil
}

// stopInForce says which stop is in force on stored, the Robot read for S6's write, or in this
// process's own state, and whether one is.
func (d *Dispatcher) stopInForce(stored *fleetv1.Robot) (string, bool) {
	if by, held := estopscope.HeldOnRecord(stored); held {
		return by.String(), true
	}
	for _, st := range stored.Status.Stops {
		if st.ClearedAt == nil {
			return "recorded stop " + st.ID, true
		}
	}
	if d.unrecordedInForce(robotEstopKey(stored.Namespace, stored.Name)) {
		return "a stop sent and not yet recorded", true
	}
	if d.Fence != nil {
		if ids := d.Fence.HeldStops(stored.Namespace, stored.Name); len(ids) > 0 {
			return "stops in force for its work " + strings.Join(ids, ", "), true
		}
	}
	return "", false
}

// storedRobot re-reads robot for setEstopState's comparison. It runs under the
// robot's write lock, so no estopState write of this dispatcher lands between the
// read and the patch. If the read fails it returns a copy of robot, and the write is
// decided against the caller's copy as it was before the re-read existed.
func (d *Dispatcher) storedRobot(ctx context.Context, robot *fleetv1.Robot) *fleetv1.Robot {
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	stored := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(robot), stored); err != nil {
		log.FromContext(ctx).Error(err, "re-reading the Robot before writing estopState; comparing against the trigger's copy",
			"robot", robot.Name)
		return robot.DeepCopy()
	}
	return stored
}

// recordSupersededOutcome records an older trigger's Failed that setEstopState did not
// write because a newer trigger's confirmed Stopped stands. The trigger's caller still
// receives Failed and audits it; this Event makes it visible on the Robot too.
func (d *Dispatcher) recordSupersededOutcome(robot *fleetv1.Robot, reason string) {
	if d.Recorder == nil {
		return
	}
	d.Recorder.Eventf(robot, corev1.EventTypeWarning, "EstopOutcomeSuperseded",
		"an earlier estop for this robot resolved Failed (%s) after a later estop was confirmed stopped; the confirmed Stopped stands",
		reason)
}

func (d *Dispatcher) recordLatencyViolation(ctx context.Context, robot *fleetv1.Robot, latency time.Duration) {
	if d.Recorder != nil {
		d.Recorder.Event(robot, corev1.EventTypeWarning, "EstopLatencyViolation",
			fmt.Sprintf("estop Command acknowledgement latency %dms exceeded %dms SLA",
				latency.Milliseconds(), d.slaThreshold.Milliseconds()))
	}
	if d.Audit == nil {
		return
	}
	// Sealed AFTER the acknowledgement, so nothing here sits between the estop and the
	// robot: the stop is already commanded and confirmed by the time this runs. The
	// record is best-effort like every other producer — an audit sink must never be able
	// to add latency to, or fail, an emergency stop.
	//
	// Outcome is Allowed, not Error: the stop was delivered and acknowledged. What
	// breached was the timing guarantee, which is a conformance fact about the adapter —
	// hence adapter_version, the field that lets an operator attribute a pattern of
	// violations to a specific build rather than to the fleet.
	if _, err := d.Audit.Record(audit.Entry{
		EventType: audit.EventEstopLatencyViolation,
		Namespace: robot.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "safety-dispatcher"},
		Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
		Action:    "estop",
		Outcome:   audit.OutcomeAllowed,
		Detail: map[string]string{
			"latency_ms":      strconv.FormatInt(latency.Milliseconds(), 10),
			"adapter_version": robot.Spec.Adapter.Version,
			"sla_ms":          strconv.FormatInt(d.slaThreshold.Milliseconds(), 10),
			"adapter":         robot.Spec.Adapter.Name,
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ESTOP_LATENCY_VIOLATION", "robot", robot.Name)
	}
}

// stillLeads reports whether this process still holds leadership (Term). One that does not
// writes no stop outcome and records nothing for a stop's answer; the skip is logged, naming
// the state not written (empty for a re-send's answer).
func (d *Dispatcher) stillLeads(robot *fleetv1.Robot, state fleetv1.RobotEstopState) bool {
	if d.Term == nil {
		return true
	}
	if _, leading := d.Term(); leading {
		return true
	}
	log.Log.WithName("estop-dispatcher").Info("stop outcome not recorded: this process no longer leads",
		"robot", robot.Name, "namespace", robot.Namespace, "state", state)
	return false
}

// NewStopID mints a stop ID: a random (version 4) UUID, 122 random bits in 128, never a
// counter, so it is unique across every control-plane process and leader term (RFC-0001
// #safety-robot-stop-table, the term stop ID). Every copy and re-send of the stop carries it.
func NewStopID() string { return string(uuid.NewUUID()) }

// Carrier names the object whose stop annotation issued a stop: the robot itself, a FleetZone or
// the namespace's SwarmadaConfig. The zero Carrier on a robot-scope stop is the robot.
type Carrier struct {
	Kind fleetv1.StopCarrierKind
	Name string
	UID  string
}

// maxRecordedStops is status.stops' cap (the CRD's MaxItems).
const maxRecordedStops = 256

// stopRecord is one stop to record in Robot.status.stops after it was sent: the entry, and the
// first STOPPING's arrival, which the recorder writes as acknowledgedAt (row S2).
type stopRecord struct {
	robot types.NamespacedName
	entry fleetv1.RobotStop
	acked chan time.Time // buffered: the first STOPPING's arrival
	done  chan struct{}  // closed when the stop's waiter returns
	once  sync.Once
	epoch uint64 // the trigger's estop epoch
	// Set by a clear that ended the stop before its record landed (guarded by Dispatcher.mu): the
	// record is written with that clearedAt, or not at all when removed (an adapter below 1.4.0,
	// or its EstopClearAck already came). clearing is open while such a clear's write is in flight.
	clearedAt *time.Time
	removed   bool
	clearing  chan struct{}
}

// clearOf is what a clear decided for r's record: its clearedAt, or that it is not written. While a
// clear that names r is being written it waits for that write, so the record can neither land
// without the clear's clearedAt nor be lost to it.
func (d *Dispatcher) clearOf(r *stopRecord) (*time.Time, bool) {
	d.mu.Lock()
	ch := r.clearing
	d.mu.Unlock()
	if ch != nil {
		<-ch
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return r.clearedAt, r.removed
}

// newStopRecord builds the record of a stop sent to robot. A zone or namespace stop with no
// carrier cannot be recorded (the CRD requires one); it is logged and recorded as nothing.
func (d *Dispatcher) newStopRecord(robot *fleetv1.Robot, id string, scope metrics.EstopScope, carrier Carrier,
	sentAt time.Time) *stopRecord {
	entry := fleetv1.RobotStop{ID: id, SentAt: metav1.NewTime(sentAt)}
	switch scope {
	case metrics.ScopeZone:
		entry.Scope = fleetv1.StopScopeZone
	case metrics.ScopeNamespace:
		entry.Scope = fleetv1.StopScopeNamespace
	default:
		entry.Scope = fleetv1.StopScopeRobot
		if carrier.Kind == "" {
			carrier = Carrier{Kind: fleetv1.StopCarrierRobot, Name: robot.Name, UID: string(robot.UID)}
		}
	}
	if carrier.Kind != "" {
		entry.Carrier = &fleetv1.StopCarrier{Kind: carrier.Kind, Name: carrier.Name, UID: carrier.UID}
	}
	return &stopRecord{robot: client.ObjectKeyFromObject(robot), entry: entry,
		acked: make(chan time.Time, 1), done: make(chan struct{})}
}

// acknowledged hands the recorder the first STOPPING's arrival; later ones are dropped.
func (r *stopRecord) acknowledged(at time.Time) {
	select {
	case r.acked <- at:
	default:
	}
}

// finish tells the recorder the waiter has returned: no STOPPING arrives after it.
func (r *stopRecord) finish() { r.once.Do(func() { close(r.done) }) }

// recordStop appends the stop to the robot's status.stops, then writes its acknowledgedAt once
// the first STOPPING has arrived. Each write carries the resourceVersion it read (a merge patch
// replaces the whole list), and is retried until it lands, the robot is gone, or this process
// no longer leads. It runs after the send, in its own goroutine, and gates nothing. At the cap
// the stop is not recorded, and a Warning Event StopRecordFull names the robot.
func (d *Dispatcher) recordStop(r *stopRecord) {
	defer d.untrackInflight(robotEstopKey(r.robot.Namespace, r.robot.Name), r.entry.ID)
	if r.entry.Carrier == nil && r.entry.Scope != fleetv1.StopScopeReported && r.entry.Scope != fleetv1.StopScopeEndState {
		log.Log.WithName("estop-dispatcher").Error(nil, "stop not recorded: a zone or namespace stop came with no carrier",
			"robot", r.robot.Name, "stopID", r.entry.ID, "scope", r.entry.Scope)
		return
	}
	// The first STOPPING may already have arrived: it is taken once, and kept across the
	// append's retries.
	var acked *time.Time
	takeAck := func() {
		if acked != nil {
			return
		}
		select {
		case at := <-r.acked:
			acked = &at
		default:
		}
	}
	appended := d.writeStopRecord(r.robot, r.entry.ID, func(robot *fleetv1.Robot) (bool, bool) {
		for i := range robot.Status.Stops {
			if robot.Status.Stops[i].ID == r.entry.ID {
				return false, false
			}
		}
		if len(robot.Status.Stops) >= maxRecordedStops {
			return false, true
		}
		e := r.entry
		takeAck()
		if acked != nil {
			t := metav1.NewTime(*acked)
			e.AcknowledgedAt = &t
		}
		clearedAt, removed := d.clearOf(r)
		if removed {
			return false, false
		}
		if clearedAt != nil {
			t := metav1.NewTime(*clearedAt)
			e.ClearedAt = &t
		}
		robot.Status.Stops = append(robot.Status.Stops, e)
		return true, false
	})
	d.mu.Lock()
	removed := r.removed
	d.mu.Unlock()
	if !appended || removed {
		return
	}
	if acked == nil {
		select {
		case at := <-r.acked:
			acked = &at
		case <-r.done:
			takeAck()
			if acked == nil {
				return
			}
		}
	}
	at := *acked
	d.writeStopRecord(r.robot, r.entry.ID, func(robot *fleetv1.Robot) (bool, bool) {
		for i := range robot.Status.Stops {
			if s := &robot.Status.Stops[i]; s.ID == r.entry.ID {
				if s.AcknowledgedAt != nil {
					return false, false
				}
				t := metav1.NewTime(at)
				s.AcknowledgedAt = &t
				return true, false
			}
		}
		return false, false
	})
}

// writeStopRecord applies change to the robot's stored status and patches it under the
// resourceVersion it read, retrying until the write lands or is not needed, the robot is gone,
// the API server refuses the record itself (invalid, bad request, forbidden), or this process no
// longer leads. change reports
// whether it changed anything, and whether the record is full. It returns whether the entry is
// on the robot (written now or already there).
func (d *Dispatcher) writeStopRecord(key types.NamespacedName, id string,
	change func(*fleetv1.Robot) (changed, full bool)) bool {
	logger := log.Log.WithName("estop-dispatcher").WithValues("robot", key.Name, "namespace", key.Namespace, "stopID", id)
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	backoff := 50 * time.Millisecond
	for {
		landed, done := d.writeStopRecordOnce(key, id, change, logger, reader)
		if done {
			return landed
		}
		time.Sleep(backoff)
		backoff = min(2*backoff, 5*time.Second)
	}
}

// writeStopRecordOnce is one read and patch of writeStopRecord. done is false only for a failure
// that is retried after a pause; a conflict is retried at once.
func (d *Dispatcher) writeStopRecordOnce(key types.NamespacedName, id string, change func(*fleetv1.Robot) (changed, full bool),
	logger logr.Logger, reader client.Reader) (landed, done bool) {
	for {
		if !d.stillLeads(&fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}, "") {
			return false, true
		}
		landed, done, err := d.writeStopRecordAttempt(key, id, change, logger, reader)
		if err == nil {
			return landed, done
		}
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("stop not recorded: the robot is gone")
			return false, true
		case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err):
			// A write the API server refuses as such is refused again: retrying it would spin.
			logger.Error(err, "stop not recorded: the API server refused the record")
			return false, true
		case apierrors.IsConflict(err):
			continue
		}
		logger.Info("recording a stop failed; retrying", "err", err.Error())
		return false, false
	}
}

// writeStopRecordAttempt is one read and patch. It takes no lock of the robot's: a record write
// that is slow or blocked delays no stop outcome (row S1).
func (d *Dispatcher) writeStopRecordAttempt(key types.NamespacedName, id string, change func(*fleetv1.Robot) (changed, full bool),
	logger logr.Logger, reader client.Reader) (landed, done bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stored := &fleetv1.Robot{}
	if err := reader.Get(ctx, key, stored); err != nil {
		return false, false, err
	}
	base := stored.DeepCopy()
	changed, full := change(stored)
	switch {
	case full:
		logger.Info("stop not recorded: status.stops is full", "entries", len(stored.Status.Stops))
		if d.Recorder != nil {
			d.Recorder.Eventf(stored, corev1.EventTypeWarning, "StopRecordFull",
				"status.stops holds %d entries, its cap; stop %s was sent but is not recorded, so a change of leader "+
					"does not carry it", len(stored.Status.Stops), id)
		}
		return false, true, nil
	case !changed:
		return true, true, nil
	}
	if err := d.Client.Status().Patch(ctx, stored, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// FenceSender sends a stop's fence on the robot's ControlStream and keeps the robot's stops in
// force, which the AssignAction send checks (command.Dispatcher satisfies it). It is an interface
// so this package does not import the command dispatcher.
type FenceSender interface {
	SendFence(robot *fleetv1.Robot, stopID string, estop *fav1.Estop) error
	ClearHeld(namespace, robot string)
	// HeldStops is the robot's stops in force for its AssignActions; S6 releases no robot with one.
	HeldStops(namespace, robot string) []string
	// RobotReleased sends what a stop withheld from the robot, once S6 has written Normal.
	RobotReleased(namespace, robot string)
}

// sendFence hands the stop's fence to Fence in a goroutine of its own, so a ControlStream that
// blocks delays nothing on the SafetyStream (row S1: a send that blocks or fails on one stream
// delays nothing on the other).
func (d *Dispatcher) sendFence(robot *fleetv1.Robot, stopID string, estop *fav1.Estop) {
	if d.Fence == nil {
		return
	}
	r := robot.DeepCopy()
	go func() {
		if err := d.Fence.SendFence(r, stopID, estop); err != nil {
			log.Log.WithName("estop-dispatcher").Info("a stop's fence was not sent on the ControlStream; it is sent when "+
				"the robot next registers on one", "robot", r.Name, "stopID", stopID, "err", err.Error())
		}
	}()
}
