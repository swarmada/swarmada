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

// Package command pushes server→adapter Commands down live ControlStreams and
// correlates the returned CommandResult by command_id (RFC-0001 §9.2). It is the
// control-plane command-push path the active-probe, lease-renew,
// and model-update flows ride on.
//
// The current consumer is the RobotProbe controller: Dispatcher implements
// [github.com/swarmada/swarmada/internal/probe.Prober]. Binding is fail-safe — a
// command that cannot be delivered or confirmed returns an error, so the caller
// resolves it to Failed/Unknown and never to a falsely-Healthy result.
package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ErrUnreachable means the Command could not be delivered or its result not
// received: no live ControlStream to the robot's adapter, a send failure, or no
// CommandResult within the window. The caller treats it as Failed, never Healthy.
var ErrUnreachable = errors.New("command not confirmed via ControlStream")

// defaultTimeout backstops a caller ctx that carries no deadline of its own.
const defaultTimeout = 10 * time.Second

// CommandSender serialises server→adapter sends on one ControlStream.
// [github.com/swarmada/swarmada/internal/controlstream] streamWriter satisfies it.
type CommandSender = controlstream.CommandSender

// Dispatcher pushes Commands to live ControlStreams and correlates their results.
// One instance serves every adapter stream; it is safe for concurrent use.
type Dispatcher struct {
	// Client resolves a robot to its managing Fleet Adapter (spec.adapter.name).
	Client client.Client
	// OnStreamRegistered, when set, is called on its own goroutine once for each verified
	// ControlStream registered, after it is registered, so a push it makes goes down that
	// stream. The manager sets it to the Zone Controller's re-send of each served robot's
	// zone rule set: an adapter process that connects or reconnects starts with none
	// (FA-R7). It is set before any stream is registered and not changed after.
	OnStreamRegistered func(identity controlstream.TLSIdentity)
	Term               LeaderTerm // stamps every Command, and sends none without leadership (term.go)
	// RequireRegister, when set (the manager sets it), sends an AssignAction only to a robot whose
	// register has been processed on its adapter's current ControlStream (fence.go).
	RequireRegister bool
	// RequireContract, when set, sends an AssignAction only on a ControlStream that negotiated
	// contract 1.4.0 (StreamNegotiated): row A15's send check, its version bullet (RFC-0001
	// #safety-action-table). The manager sets it from --require-latch-contract, off at v0.4: the
	// in-tree deployments run the simulation adapter at 1.3.0.
	RequireContract bool
	// APIReader reads the API server past the cache. The motion gate (fence.go) reads the Robot
	// through it, so a stop state written a moment earlier is seen. Nil falls back to Client.
	APIReader client.Reader
	// Recorder raises an Event on a Robot whose fences could not be sent on a new ControlStream.
	// Nil logs only.
	Recorder record.EventRecorder
	now      func() time.Time
	timeout  time.Duration

	mu        sync.Mutex
	streams   map[string]CommandSender  // adapterKey → live sender
	pending   map[uint64]pendingCommand // command_id → the command's waiter
	beats     map[string]chan struct{}  // namespace/wire robot_id → heartbeat waiter
	idCounter uint64
	fence     fenceState // stops in force and readiness per robot (fence.go)
}

// pendingCommand is a pushed Command awaiting its result, with the stream it went down
// and the robot it was pushed for. A result must come back the same way to resolve it.
// robotID is the wire robot_id the Command carried (robotid.Wire), which is what the
// adapter's result names.
type pendingCommand struct {
	sender  CommandSender
	robotID string
	ch      chan *fav1.CommandResult
}

var _ probe.Prober = (*Dispatcher)(nil)
var _ controlstream.CommandDispatcher = (*Dispatcher)(nil)

// New builds a command Dispatcher.
func New(c client.Client) *Dispatcher {
	return &Dispatcher{
		Client:  c,
		timeout: defaultTimeout,
		streams: map[string]CommandSender{},
		pending: map[uint64]pendingCommand{},
		beats:   map[string]chan struct{}{},
	}
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func adapterKey(namespace, adapter string) string { return namespace + "/" + adapter }

// RegisterStream records a live ControlStream for an adapter identity and returns
// a deregister func to call when the stream closes. An unverified identity is not
// registered — nothing may be pushed to an unauthenticated stream.
//
// Registering also starts this stream's adapter liveness cadence (see
// adapter_liveness.go): the control plane owns the heartbeat direction by contract, so a
// registered stream is exactly the scope over which the periodic HeartbeatRequest is
// defined. The returned deregister stops it, so no beat outlives its stream. It then
// calls OnStreamRegistered, when set.
func (d *Dispatcher) RegisterStream(identity controlstream.TLSIdentity, sender CommandSender) func() {
	if !identity.Verified {
		return func() {}
	}
	key := adapterKey(identity.Namespace, identity.AdapterName)
	d.mu.Lock()
	d.streams[key] = sender
	d.mu.Unlock()
	stopBeats := d.startAdapterBeats(identity, sender)
	if d.OnStreamRegistered != nil {
		go d.OnStreamRegistered(identity)
	}
	return func() {
		stopBeats()
		d.mu.Lock()
		if d.streams[key] == sender {
			delete(d.streams, key)
		}
		d.mu.Unlock()
		d.forgetStream(sender)
	}
}

// RouteResult delivers a CommandResult to the waiting caller, correlated by command_id.
// It never blocks the ControlStream receive loop (buffered channel); a result for an
// unknown/expired command_id is dropped and returns nil.
//
// command_id is a per-process counter, so it is easy to guess, and the robot_id gate in
// front of this only proves the sender serves the robot it names. So a result resolves
// its command only when it arrived on the stream the command was pushed down (sender)
// and names the robot the command was pushed for. Anything else is dropped, and the
// returned error, wrapping controlstream.ErrMisroutedResult, is the caller's to log and
// audit (RFC-0001 #security-fleet-adapter-authentication-robot-identity).
func (d *Dispatcher) RouteResult(sender CommandSender, result *fav1.CommandResult) error {
	if result == nil {
		return nil
	}
	id := result.GetCommandId()
	d.mu.Lock()
	p, ok := d.pending[id]
	d.mu.Unlock()
	if !ok {
		return nil
	}
	if p.sender != sender {
		return fmt.Errorf("%w: command %d was pushed down a different ControlStream", controlstream.ErrMisroutedResult, id)
	}
	if result.GetRobotId() != p.robotID {
		return fmt.Errorf("%w: command %d was pushed for robot %q, the result names %q",
			controlstream.ErrMisroutedResult, id, p.robotID, result.GetRobotId())
	}
	select {
	case p.ch <- result:
	default:
	}
	return nil
}

// Verify pushes a verify_* Command to robotID's adapter and binds the returned
// VerifyResult (RFC-0001 #robotprobe-the-verify-probe-commands). A missing stream, send failure, or absent
// result returns ErrUnreachable — the caller resolves that to Failed, never
// Healthy. An adapter that declines the probe (CommandResult.unsupported) returns
// a non-error Result with Unsupported=true.
func (d *Dispatcher) Verify(ctx context.Context, namespace, robotID string, req probe.VerifyRequest) (probe.Result, error) {
	cmd, err := buildVerifyCommand(req)
	if err != nil {
		return probe.Result{}, err
	}
	res, err := d.roundTrip(ctx, namespace, robotID, cmd)
	if err != nil {
		return probe.Result{}, err
	}
	return bindResult(res)
}

// roundTrip resolves robotID to its adapter's live ControlStream, pushes cmd
// (stamping its robot_id and a fresh command_id), and waits for the correlated
// CommandResult. A missing stream, send failure, or no result within the window
// returns ErrUnreachable; a cancelled ctx returns its error. It is the shared
// send-and-correlate core for every command-push flow (verify_*, model_update, …).
//
// robotID is the Robot's object name; callers name robots the way the control plane
// stores them. The Command carries the robot's wire robot_id instead, the id its adapter
// announced (robotid.Wire), because that is the only id the adapter knows the robot by.
func (d *Dispatcher) roundTrip(ctx context.Context, namespace, robotID string, cmd *fav1.Command) (*fav1.CommandResult, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, robot); err != nil {
		return nil, fmt.Errorf("command: robot %q lookup: %w", robotID, err)
	}
	return d.roundTripTo(ctx, robot, cmd)
}

// roundTripTo is roundTrip for a Robot already read. The wire robot_id is resolved once,
// here, and the result is matched on that id: a Command in flight when the annotation is
// re-pointed is answered under the id it was sent to or not at all, and then it returns
// ErrUnreachable at the end of the window.
func (d *Dispatcher) roundTripTo(ctx context.Context, robot *fleetv1.Robot, cmd *fav1.Command) (*fav1.CommandResult, error) {
	namespace, wireID := robot.Namespace, robotid.Wire(robot)
	key := adapterKey(namespace, robot.Spec.Adapter.Name)
	d.mu.Lock()
	sender := d.streams[key]
	d.mu.Unlock()
	if sender == nil {
		return nil, fmt.Errorf("%w: no ControlStream to adapter %q", ErrUnreachable, robot.Spec.Adapter.Name)
	}

	id, ch := d.register(sender, wireID)
	defer d.deregister(id)
	cmd.RobotId = wireID
	cmd.CommandId = id

	// The one send site of every Command: d.send stamps the leader term, and sends nothing
	// from a process that does not hold leadership (term.go).
	if err := d.send(sender, cmd); err != nil {
		return nil, err
	}

	select {
	case res := <-ch:
		return termChecked(ctx, robot, cmd, res)
	case <-time.After(d.timeout):
		return nil, fmt.Errorf("%w: no CommandResult within %s", ErrUnreachable, d.timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RouteHeartbeat delivers an inbound HeartbeatResponse to a waiting Heartbeat call.
// robotID is the wire robot_id the response names.
//
// Correlation is by robot_id, not by a command id: HeartbeatRequest/Response carry no
// correlation field, and adding one would be a wire change for a question that does not
// need it. Any response naming the robot within the window answers "is this robot alive",
// which is the only thing the caller is asking — a response to an earlier request is not a
// stale answer, it is the same fact arriving slightly late.
func (d *Dispatcher) RouteHeartbeat(namespace, robotID string) {
	if robotID == "" {
		return
	}
	d.mu.Lock()
	ch := d.beats[adapterKey(namespace, robotID)]
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default: // a waiter already satisfied; nothing to do
		}
	}
}

// Heartbeat pushes a HeartbeatRequest for the Robot named robotID, under its wire robot_id
// (robotid.Wire), and reports whether the adapter answered
// within the dispatcher's timeout (RFC-0001 #safety-control-plane-response — the confirming exchange before a
// robot is declared Offline).
//
// Three outcomes, and the caller must not collapse them:
//   - (true, nil)  — the robot answered; it is alive despite stale telemetry.
//   - (false, nil) — the stream is live but nothing answered in time. That is evidence.
//   - (false, err) — wrapped ErrUnreachable: no stream at all, or the send failed. Also
//     evidence of disconnection, but of a different kind, and worth logging distinctly.
func (d *Dispatcher) Heartbeat(ctx context.Context, namespace, robotID string) (bool, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, robot); err != nil {
		return false, fmt.Errorf("heartbeat: robot %q lookup: %w", robotID, err)
	}

	d.mu.Lock()
	sender := d.streams[adapterKey(namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		return false, fmt.Errorf("%w: no ControlStream to adapter %q", ErrUnreachable, robot.Spec.Adapter.Name)
	}

	wireID := robotid.Wire(robot)
	key := adapterKey(namespace, wireID)
	ch := make(chan struct{}, 1)
	d.mu.Lock()
	d.beats[key] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.beats[key] == ch {
			delete(d.beats, key)
		}
		d.mu.Unlock()
	}()

	if err := sender.Send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_Heartbeat{Heartbeat: &fav1.HeartbeatRequest{
			RobotId: wireID, SentAtMs: d.clock().UnixMilli(),
		}},
	}); err != nil {
		return false, fmt.Errorf("%w: send: %v", ErrUnreachable, err)
	}

	select {
	case <-ch:
		return true, nil
	case <-time.After(d.timeout):
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

var _ controlstream.ResourceGrantNotifier = (*Dispatcher)(nil)

// PushReservationGranted pushes a reservation_granted Command to robotID's adapter
// and binds the ReservationGrantedAck (RFC-0001 #tde-shared-resource-queue-management). A missing stream / send failure /
// timeout returns ErrUnreachable (wrapped) — the caller knows the promoted robot
// was not told to proceed, so it stays held at the boundary (fail-safe).
//
// Unlike the other pushes, robotID is a WIRE robot_id, not an object name: an adapter
// reserves a resource under the robot_id it announced, and the queue promotes that id.
// It is resolved to its Robot through the annotation (robotid.Lookup).
func (d *Dispatcher) PushReservationGranted(ctx context.Context, namespace, robotID, resourceName string) (bool, error) {
	cmd := &fav1.Command{Command: &fav1.Command_ReservationGranted{
		ReservationGranted: &fav1.ReservationGranted{ResourceName: resourceName},
	}}
	robot, err := robotid.Lookup(ctx, d.Client, namespace, robotID)
	if err != nil {
		return false, fmt.Errorf("command: %w", err)
	}
	if robot == nil {
		return false, fmt.Errorf("command: no Robot in namespace %q carries robot_id %q", namespace, robotID)
	}
	// A grant lets the robot move on, so a stop that holds it withholds the grant until the
	// robot is released (fence.go, roundTripMotion).
	res, err := d.roundTripMotion(ctx, robot, motion{resource: resourceName, robotID: robotID}, cmd)
	if err != nil {
		return false, err
	}
	return res.GetReservationGranted().GetAcknowledged(), nil
}

// NotifyReservationGranted satisfies controlstream.ResourceGrantNotifier. It pushes
// the grant asynchronously and detached from the releasing robot's stream ctx, so
// the release response is not blocked on the promoted robot's ack and a closing
// release stream does not cancel a push to a different adapter. Best-effort: a
// delivery failure leaves the promoted robot waiting at the boundary (fail-safe),
// and the promotion is already persisted to FleetZone.status.
func (d *Dispatcher) NotifyReservationGranted(_ context.Context, namespace, robotID, resourceName string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()
		if _, err := d.PushReservationGranted(ctx, namespace, robotID, resourceName); errors.Is(err, ErrWithheldByStop) {
			log.Log.WithName("command-dispatcher").Info("reservation_granted withheld until the robot is released",
				"namespace", namespace, "robot", robotID, "resource", resourceName, "reason", err.Error())
		}
	}()
}

// PushZoneAdmission pushes a zone_admission Command (hold/admit at a leaf-zone
// boundary, RFC-0001 #control-plane-zone-controller/RFC-0001 #control-plane-zone-controller) and binds the ZoneAdmissionAck. A missing stream / send
// failure / timeout returns ErrUnreachable (wrapped) — the boundary hold/admit was
// not delivered.
//
// An admit lets the robot move on, so a stop that holds the robot withholds it until the robot is
// released (ErrWithheldByStop; fence.go, roundTripMotion). A hold moves nothing and is never
// withheld; it drops an admit withheld for the same zone, so the release does not send it.
func (d *Dispatcher) PushZoneAdmission(ctx context.Context, namespace, robotID, zoneName string, admit bool) (bool, error) {
	cmd := &fav1.Command{Command: &fav1.Command_ZoneAdmission{
		ZoneAdmission: &fav1.ZoneAdmission{ZoneName: zoneName, Admit: admit},
	}}
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, robot); err != nil {
		return false, fmt.Errorf("command: robot %q lookup: %w", robotID, err)
	}
	var res *fav1.CommandResult
	var err error
	if admit {
		res, err = d.roundTripMotion(ctx, robot, motion{zone: zoneName}, cmd)
	} else {
		res, err = d.roundTripHold(ctx, robot, motion{zone: zoneName}, cmd)
	}
	if err != nil {
		return false, err
	}
	return res.GetZoneAdmission().GetAcknowledged(), nil
}

// NotifyZoneAdmission satisfies controller.ZoneAdmissionNotifier: an async,
// best-effort push detached from the caller's ctx (a hold/admit is advisory, so the
// Zone Controller must not block on the ack; a closing observer ctx must not cancel
// a push to a robot's own adapter stream). A delivery failure is dropped — the
// per-zone occupancy re-evaluates and re-pushes on the next transition.
func (d *Dispatcher) NotifyZoneAdmission(_ context.Context, namespace, robotID, zoneName string, admit bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()
		if _, err := d.PushZoneAdmission(ctx, namespace, robotID, zoneName, admit); errors.Is(err, ErrWithheldByStop) {
			log.Log.WithName("command-dispatcher").Info("zone_admission admit withheld until the robot is released",
				"namespace", namespace, "robot", robotID, "zone", zoneName, "reason", err.Error())
		}
	}()
}

// ModelUpdate is the payload for a server→adapter model_update Command (RFC-0001
// RFC-0001 #control-plane-ota-model-update-manager): it tells the adapter to fetch, verify, and install a model version.
type ModelUpdate struct {
	ModelName         string
	OldVersion        string
	NewVersion        string
	ModelURI          string
	ModelChecksum     string
	ModelSignatureRef string
	// Rollback marks an Auto rollback (§6.7): the adapter reactivates the
	// robot-retained NewVersion without downloading (ModelURI empty).
	Rollback bool
}

// ModelUpdateOutcome is the adapter's model_update acknowledgement. Acknowledged
// means the adapter accepted the update for install (it later reports Active via
// telemetry); it does NOT mean the install has completed.
type ModelUpdateOutcome struct {
	Acknowledged   bool
	VerifiedSigner string
	Message        string
}

// ModelUpdatePusher pushes a model_update Command to a robot's adapter and returns
// its acknowledgement. [Dispatcher] satisfies it. A non-nil error means the update
// could not be delivered (unreachable/timeout) — the caller must not treat the
// robot as updating.
type ModelUpdatePusher interface {
	PushModelUpdate(ctx context.Context, namespace, robotID string, u ModelUpdate) (ModelUpdateOutcome, error)
}

var _ ModelUpdatePusher = (*Dispatcher)(nil)

// PushModelUpdate pushes a model_update Command to robotID's adapter and binds the
// returned ModelUpdateResult. A missing stream / send failure / timeout returns
// ErrUnreachable (wrapped); the caller then leaves the robot out of the batch.
func (d *Dispatcher) PushModelUpdate(ctx context.Context, namespace, robotID string, u ModelUpdate) (ModelUpdateOutcome, error) {
	cmd := &fav1.Command{Command: &fav1.Command_ModelUpdate{ModelUpdate: &fav1.ModelUpdate{
		ModelName:         u.ModelName,
		OldVersion:        u.OldVersion,
		NewVersion:        u.NewVersion,
		ModelUri:          u.ModelURI,
		ModelChecksum:     u.ModelChecksum,
		ModelSignatureRef: u.ModelSignatureRef,
		Rollback:          u.Rollback,
	}}}
	res, err := d.roundTrip(ctx, namespace, robotID, cmd)
	if err != nil {
		return ModelUpdateOutcome{}, err
	}
	if res.GetUnsupported() {
		return ModelUpdateOutcome{}, fmt.Errorf("%w: adapter does not support model_update", ErrUnreachable)
	}
	mu := res.GetModelUpdate()
	if mu == nil {
		return ModelUpdateOutcome{}, fmt.Errorf("%w: CommandResult missing model_update payload", ErrUnreachable)
	}
	return ModelUpdateOutcome{
		Acknowledged:   mu.GetAcknowledged(),
		VerifiedSigner: mu.GetVerifiedSigner(),
		Message:        mu.GetMessage(),
	}, nil
}

// ValidateOutcome is the result of an adapter action-validity check. Unsupported
// marks an adapter that does not implement validate_action — the action is then
// UNCONFIRMED for that adapter, distinct from an affirmative not-servable.
type ValidateOutcome struct {
	Servable    bool
	Unsupported bool
	Message     string
}

// ActionValidator confirms whether an adapter can serve an action's type+payload
// (RFC-0001 §9.2). [Dispatcher] satisfies it. Used at FleetAction/FleetTask
// admission and re-checked at assignment; a missing stream or an `unsupported`
// reply both leave the action UNCONFIRMED — never falsely servable.
type ActionValidator interface {
	ValidateAction(ctx context.Context, namespace, robotID, actionType, customType string, payloadJSON []byte) (ValidateOutcome, error)
}

var _ ActionValidator = (*Dispatcher)(nil)

// ValidateAction pushes a validate_action Command to robotID's adapter and binds
// the ValidateActionResult. Pure inspection — reserves nothing, moves no robot. A
// missing stream / send failure / timeout returns ErrUnreachable (wrapped); an
// adapter replying `unsupported` yields ValidateOutcome{Unsupported: true}.
//
// customType is the Custom verb (spec.customType), carried as ValidateAction.custom_type so
// the adapter judges the verb the assignment will carry; empty for every other type.
func (d *Dispatcher) ValidateAction(ctx context.Context, namespace, robotID, actionType, customType string, payloadJSON []byte) (ValidateOutcome, error) {
	cmd := &fav1.Command{Command: &fav1.Command_ValidateAction{ValidateAction: &fav1.ValidateAction{
		ActionType:  actionType,
		PayloadJson: payloadJSON,
		CustomType:  customType,
	}}}
	res, err := d.roundTrip(ctx, namespace, robotID, cmd)
	if err != nil {
		return ValidateOutcome{}, err
	}
	if res.GetUnsupported() {
		return ValidateOutcome{Unsupported: true, Message: "adapter does not implement validate_action"}, nil
	}
	v := res.GetValidateAction()
	if v == nil {
		return ValidateOutcome{}, fmt.Errorf("%w: CommandResult missing validate_action payload", ErrUnreachable)
	}
	return ValidateOutcome{Servable: v.GetServable(), Message: v.GetMessage()}, nil
}

// buildVerifyCommand assembles the verify_* Command arm for a probe type. The
// command_id/robot_id are stamped by the caller.
func buildVerifyCommand(req probe.VerifyRequest) (*fav1.Command, error) {
	cmd := &fav1.Command{}
	switch req.ProbeType {
	case fleetv1.ProbeTypeHardware:
		cmd.Command = &fav1.Command_VerifyHardware{VerifyHardware: &fav1.VerifyHardware{
			ComponentName: req.Target, ExpectedMetrics: req.Expected,
		}}
	case fleetv1.ProbeTypeCapability:
		cmd.Command = &fav1.Command_VerifyCapability{VerifyCapability: &fav1.VerifyCapability{
			CapabilityName: req.Target, ExpectedMetrics: req.Expected,
		}}
	case fleetv1.ProbeTypeModel:
		cmd.Command = &fav1.Command_VerifyModel{VerifyModel: &fav1.VerifyModel{
			ModelName: req.Target, SyntheticInput: req.SyntheticInput, ExpectedMetrics: req.Expected,
		}}
	default:
		return nil, fmt.Errorf("unknown probe type %q", req.ProbeType)
	}
	return cmd, nil
}

// bindResult maps a CommandResult into the probe.Result. An `unsupported` result
// binds to Unsupported (the caller reports Unknown). A result missing the verify
// arm is malformed and returns an error (Failed, never Healthy).
func bindResult(res *fav1.CommandResult) (probe.Result, error) {
	if res.GetUnsupported() {
		return probe.Result{Unsupported: true, Message: "adapter declined the probe"}, nil
	}
	vr := res.GetVerify()
	if vr == nil {
		return probe.Result{}, fmt.Errorf("%w: CommandResult missing verify payload", ErrUnreachable)
	}
	return probe.Result{
		Status:        probe.BindProbeStatus(vr.GetStatus()),
		ActualMetrics: copyMetrics(vr.GetActualMetrics()),
		Message:       vr.GetMessage(),
	}, nil
}

func copyMetrics(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (d *Dispatcher) register(sender CommandSender, robotID string) (id uint64, ch chan *fav1.CommandResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.idCounter++
	ch = make(chan *fav1.CommandResult, 4)
	d.pending[d.idCounter] = pendingCommand{sender: sender, robotID: robotID, ch: ch}
	return d.idCounter, ch
}

func (d *Dispatcher) deregister(id uint64) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}
