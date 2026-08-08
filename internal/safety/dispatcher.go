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
// confirmed outcome (RFC-0001 §9.6.2). The cardinal invariant: a robot is treated
// as STOPPED only when the Fleet Adapter CONFIRMS it via EstopAck.state=STOPPED —
// never inferred from silence, a dropped message, or a timeout. Anything the
// control plane cannot confirm resolves to Failed (escalate), never Stopped.
package safety

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ErrUndelivered means the estop could not be confirmed via the SafetyStream
// (no connected stream, send failure, or no EstopAck). The robot is left Failed
// and MUST be escalated (edge/manual) — it is NOT stopped.
var ErrUndelivered = errors.New("estop not confirmed via SafetyStream; escalate")

// Default timing (§9.6.2.2/.3).
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
	// Audit seals ESTOP_LATENCY_VIOLATION (§9.6.5.1) into the tamper-evident chain.
	// Nil disables recording; estop delivery is unaffected.
	Audit audit.Recorder

	now             func() time.Time
	slaThreshold    time.Duration
	confirmTimeout  time.Duration
	deliveryTimeout time.Duration

	mu        sync.Mutex
	streams   map[string]controlstream.SafetySender // adapterKey → live sender
	pending   map[string]chan *fav1.EstopAck        // estopID → ack channel
	idCounter uint64
}

// New builds a Dispatcher with the §9.6.2 default timings.
func New(c client.Client, recorder record.EventRecorder) *Dispatcher {
	return &Dispatcher{
		Client:          c,
		Recorder:        recorder,
		slaThreshold:    defaultSLAThreshold,
		confirmTimeout:  defaultConfirmTimeout,
		deliveryTimeout: defaultDeliveryTimeout,
		streams:         map[string]controlstream.SafetySender{},
		pending:         map[string]chan *fav1.EstopAck{},
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
func (d *Dispatcher) RegisterStream(identity controlstream.TLSIdentity, sender controlstream.SafetySender) func() {
	if !identity.Verified {
		return func() {}
	}
	key := adapterKey(identity.Namespace, identity.AdapterName)
	d.mu.Lock()
	d.streams[key] = sender
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		if d.streams[key] == sender {
			delete(d.streams, key)
		}
		d.mu.Unlock()
	}
}

// RouteAck delivers an EstopAck to the waiting TriggerEstop, correlated by
// estop_id. It never blocks the SafetyStream receive loop (buffered channel).
func (d *Dispatcher) RouteAck(ack *fav1.EstopAck) {
	if ack == nil {
		return
	}
	d.mu.Lock()
	ch := d.pending[ack.GetEstopId()]
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ack:
		default:
		}
	}
}

// TriggerEstop pushes an estop to robotID's adapter and resolves its confirmed
// state. It writes Robot.status.estopState=Stopped ONLY on a confirmed
// EstopAck.state=STOPPED; a dropped estop, silence, a send failure, an adapter
// FAILED, or STOPPING-without-STOPPED all resolve to Failed (escalate), never
// Stopped. Estop is never fenced — no lease/priority gate.
func (d *Dispatcher) TriggerEstop(ctx context.Context, namespace, robotID, reason, issuedBy string) (Result, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return Result{}, fmt.Errorf("estop: robot %q lookup: %w", robotID, err)
	}

	adapter := robot.Spec.Adapter.Name
	key := adapterKey(namespace, adapter)
	d.mu.Lock()
	sender := d.streams[key]
	d.mu.Unlock()
	if sender == nil {
		// No SafetyStream to the adapter — cannot confirm via the control plane.
		d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "no SafetyStream to adapter "+robot.Spec.Adapter.Name)
		return Result{State: fleetv1.RobotEstopFailed}, ErrUndelivered
	}

	estopID, seq := d.register(robotID)
	defer d.deregister(estopID)
	ch := d.pendingChan(estopID)

	sentAt := d.clock()
	err := sender.Send(&fav1.ControlPlaneSafetyMessage{
		Seq:     seq,
		RobotId: robotID,
		Payload: &fav1.ControlPlaneSafetyMessage_Estop{Estop: &fav1.Estop{
			EstopId: estopID, Reason: reason, IssuedBy: issuedBy, IssuedAtMs: sentAt.UnixMilli(),
		}},
	})
	if err != nil {
		d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "estop send failed: "+err.Error())
		return Result{State: fleetv1.RobotEstopFailed}, fmt.Errorf("estop send: %w", err)
	}

	// First ack (delivery window). The per-adapter ACK wait is namespace-tunable
	// (spec.estop.delivery.perAdapterTimeoutMs, ADR-0016), fail-safe to the built-in
	// default. No ack ⇒ dropped ⇒ Failed, NEVER Stopped.
	deliveryTimeout := d.deliveryTimeoutFor(ctx, namespace)
	var first *fav1.EstopAck
	select {
	case first = <-ch:
	case <-time.After(deliveryTimeout):
		// Sent but no EstopAck: a Command that never acked (§9.3.8 result=timeout).
		metrics.IncEstopCommand(namespace, adapter, metrics.ScopeRobot, metrics.ResultTimeout)
		d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "no EstopAck within delivery window (dropped estop)")
		return Result{State: fleetv1.RobotEstopFailed}, ErrUndelivered
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	latency := d.clock().Sub(sentAt)
	violation := latency > d.slaThreshold
	// §9.3.8: record the round-trip latency for every ack'd estop, and the SLA
	// violation counter alongside the existing EstopLatencyViolation event.
	metrics.ObserveEstopLatency(namespace, adapter, robotID, metrics.ScopeRobot, latency)
	if violation {
		metrics.IncEstopLatencyViolation(namespace, adapter, robotID)
		d.recordLatencyViolation(ctx, robot, latency)
	}
	res := Result{Delivered: true, Latency: latency, LatencyViolation: violation}

	// Confirmed-only STOPPED. The commands_total result reflects this first ack's
	// disposition (§9.3.8); the confirm loop below governs the resulting estopState
	// but is not double-counted.
	switch first.GetState() {
	case fav1.EstopState_ESTOP_STATE_STOPPED:
		metrics.IncEstopCommand(namespace, adapter, metrics.ScopeRobot, metrics.ResultAckStopped)
		d.setEstopState(ctx, robot, fleetv1.RobotEstopStopped, "confirmed stopped")
		res.Confirmed, res.State = true, fleetv1.RobotEstopStopped
		return res, nil
	case fav1.EstopState_ESTOP_STATE_FAILED:
		metrics.IncEstopCommand(namespace, adapter, metrics.ScopeRobot, metrics.ResultAckFailed)
		d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "adapter reported FAILED: "+first.GetMessage())
		res.State = fleetv1.RobotEstopFailed
		return res, nil
	default: // STOPPING (or unspecified): stop commanded; await CONFIRMED STOPPED.
		metrics.IncEstopCommand(namespace, adapter, metrics.ScopeRobot, metrics.ResultAckStopping)
		d.setEstopState(ctx, robot, fleetv1.RobotEstopStopping, "stop commanded; awaiting confirmation")
	}

	deadline := time.After(d.confirmTimeout)
	for {
		select {
		case ack := <-ch:
			switch ack.GetState() {
			case fav1.EstopState_ESTOP_STATE_STOPPED:
				d.setEstopState(ctx, robot, fleetv1.RobotEstopStopped, "confirmed stopped")
				res.Confirmed, res.State = true, fleetv1.RobotEstopStopped
				return res, nil
			case fav1.EstopState_ESTOP_STATE_FAILED:
				d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "adapter reported FAILED")
				res.State = fleetv1.RobotEstopFailed
				return res, nil
			}
			// another STOPPING — keep waiting for a CONFIRMED stop
		case <-deadline:
			// §9.6.2.3: no STOPPED confirmation within the window ⇒ FAILED. The
			// robot is NEVER assumed stopped.
			d.setEstopState(ctx, robot, fleetv1.RobotEstopFailed, "no STOPPED confirmation within confirm window")
			res.State = fleetv1.RobotEstopFailed
			return res, nil
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

func (d *Dispatcher) register(robotID string) (estopID string, seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.idCounter++
	estopID = fmt.Sprintf("estop-%s-%d", robotID, d.idCounter)
	d.pending[estopID] = make(chan *fav1.EstopAck, 4)
	return estopID, d.idCounter
}

func (d *Dispatcher) pendingChan(estopID string) chan *fav1.EstopAck {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[estopID]
}

func (d *Dispatcher) deregister(estopID string) {
	d.mu.Lock()
	delete(d.pending, estopID)
	d.mu.Unlock()
}

// ClearEstop resets a robot's estop to Normal on an operator-authorized clear
// (§9.6.2.3 — "awaiting explicit operator clear"). It is a no-op when the robot is
// not under an estop. It clears ONLY the robot's estopState: a FleetAction that was
// Paused by the estop stays operator-gated and is NOT auto-resumed (the operator
// requeues/resumes the action separately). Returns the resulting state.
func (d *Dispatcher) ClearEstop(ctx context.Context, namespace, robotID, clearedBy string) (fleetv1.RobotEstopState, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, robot); err != nil {
		return "", fmt.Errorf("estop-clear: robot %q lookup: %w", robotID, err)
	}
	if robot.Status.EstopState == "" || robot.Status.EstopState == fleetv1.RobotEstopNormal {
		return fleetv1.RobotEstopNormal, nil // nothing to clear
	}
	d.setEstopState(ctx, robot, fleetv1.RobotEstopNormal, "cleared by "+clearedBy)
	return fleetv1.RobotEstopNormal, nil
}

// setEstopState writes a confirmed estop transition to Robot.status.estopState.
// It is a material safety transition (never a telemetry tick — RA-1) and a no-op
// when the state is unchanged.
func (d *Dispatcher) setEstopState(ctx context.Context, robot *fleetv1.Robot, state fleetv1.RobotEstopState, reason string) {
	if robot.Status.EstopState == state {
		return
	}
	base := robot.DeepCopy()
	robot.Status.EstopState = state
	if err := d.Client.Status().Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "writing estopState", "robot", robot.Name, "state", state, "reason", reason)
	}
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
