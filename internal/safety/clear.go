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

package safety

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The clear (RFC-0001 #safety-robot-stop-table, row S5; Removing a recorded stop).
//
// A clear is one robots/status write, under a resourceVersion precondition on the Robot it read:
// Resuming, the estopHeldBy records it ends, and clearedAt on every stop recorded on the robot that
// has ended. Only once that write has landed does the control plane send, on the robot's
// SafetyStream, an EstopClear naming each of those stops. An entry is removed once the adapter
// answers its EstopClear with an EstopClearAck; on a connection whose adapter reported a contract
// below 1.4.0 the clear removes it, and sends nothing. On one whose version this process does not
// know (a new leader, before the adapter's next ControlStream) or does not support, the clear sends
// nothing and keeps the entry until the adapter's report decides. Each SafetyStream that opens while
// an entry awaits its EstopClearAck is sent that EstopClear again.

// ErrNotLeading means the clear was not written, and nothing was sent, because this process does
// not hold leadership.
var ErrNotLeading = errors.New("this control-plane process does not lead; nothing written or sent")

// Ends is what a clear ends on a robot beside the robot's own stop: the status.estopHeldBy records
// it drops, and the zone and namespace stops recorded in status.stops whose carrier no longer
// carries a stop that reaches the robot. The zero Ends ends only the robot's own stop.
type Ends struct {
	Hold func(fleetv1.EstopHold) bool
	Stop func(fleetv1.RobotStop) bool
}

// clearStopsMinContract is the first contract version whose adapters answer an EstopClear.
const clearStopsMinContract = "1.4.0"

// ContractReported records the contract version an adapter reported on its ControlStream
// (controlstream.ContractObserver). A version below 1.4.0 has no EstopClear: the entries of that
// adapter's robots that await an EstopClearAck are removed now, and later clears remove theirs. A
// version of 1.4.0 or later sends, on the adapter's SafetyStream, the EstopClear of each entry
// that awaits one: a clear taken while the version was not known here sent none.
func (d *Dispatcher) ContractReported(identity controlstream.TLSIdentity, version string) {
	if !identity.Verified {
		return
	}
	d.mu.Lock()
	if d.contracts == nil {
		d.contracts = map[string]string{}
	}
	d.contracts[adapterKey(identity.Namespace, identity.AdapterName)] = version
	d.mu.Unlock()
	switch contractClearPath(version) {
	case clearRemove:
		go d.removeAwaitingClears(identity)
	case clearSend:
		go d.sendAwaitingClears(identity)
		go d.answerKeptReports(identity)
	}
}

// clearPath is what a clear does with the stops it ends, by the contract version the robot's
// adapter reported on its ControlStream to this process.
type clearPath int

const (
	// clearHold: no version known here (a new leader has none until the adapter's next
	// ControlStream), or one this control plane does not support. A version it cannot read is
	// never an implicit pass, and an EstopClear is a release: none is sent. The entry is kept with
	// its clearedAt, holds nothing, and waits for the adapter's report (ContractReported).
	clearHold clearPath = iota
	// clearRemove: 1.3.x. The connection has no EstopClear: the entry is removed at the clear.
	clearRemove
	// clearSend: 1.4.0. The EstopClear is sent, and the entry awaits its EstopClearAck.
	clearSend
)

// contractClearPath classifies a reported contract version for the clear.
func contractClearPath(version string) clearPath {
	if ok, _ := contract.Supports(version); !ok {
		return clearHold
	}
	if below, ok := contract.Below(version, clearStopsMinContract); !ok {
		return clearHold
	} else if below {
		return clearRemove
	}
	return clearSend
}

// adapterClearPath is the clear's path for the robot's adapter, by the version it last reported
// to this process; clearHold when it has reported none.
func (d *Dispatcher) adapterClearPath(robot *fleetv1.Robot) clearPath {
	d.mu.Lock()
	v, ok := d.contracts[adapterKey(robot.Namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if !ok {
		return clearHold
	}
	return contractClearPath(v)
}

// leads reports whether this process holds leadership (Term), and its term.
func (d *Dispatcher) leads() (uint64, bool) {
	if d.Term == nil {
		return 0, true
	}
	return d.Term()
}

// ClearEstop is row S5: the authorized clear of the last stop in force on a robot moves it from
// Stopped or Failed to Resuming, the post-clear supervised window (RFC-0001
// #safety-estop-state-machine). Only an operator's later release (ConfirmResume) returns it to
// Normal; a FleetAction Paused by the stop stays operator-gated.
//
// The guard is checked on the Robot read past the cache, under the robot's write lock, and again
// on every retry:
//   - a trigger issued after this clear supersedes it;
//   - a Stopping robot is not moved: the stop's outcome is not known yet;
//   - no other stop is in force on the robot: its own trigger, an estopHeldBy record ends does not
//     drop, a recorded stop with no clearedAt that has not ended, or a stop this process sent and
//     has not recorded yet whose carrier still carries it.
//
// The write carries a resourceVersion precondition. A conflict reads the Robot again and checks
// the guard again: a stop recorded since makes the clear write nothing and send nothing, and the
// state that stands is returned. Only once the write has landed does it end the robot's stops in
// force for its AssignActions (Fence), and send one EstopClear per stop it ended, stamped with the
// leader term. A process that does not lead writes nothing, sends nothing and returns ErrNotLeading.
//
// A Robot that reads Normal and records a zone or namespace stop was under that stop, though its
// stop state was never written. It is moved to Resuming like a stopped robot, so it too waits for
// an operator's release.
//
// The write is licensed under RA-1 (docs/architecture.md) as a MATERIAL TRANSITION: an operator
// action through the estop-clear verb, at most once per estop episode.
//
// A caller MUST NOT read a nil error as "the robot is clear", nor Resuming as "the robot is ready
// for work": a Resuming robot is withheld from dispatch (internal/controller/adapter_readiness.go).
func (d *Dispatcher) ClearEstop(ctx context.Context, namespace, robotID, clearedBy string, ends Ends) (fleetv1.RobotEstopState, error) {
	key := robotEstopKey(namespace, robotID)
	// This clear's estop epoch, taken before any read, so that a trigger issued while the clear is
	// in flight is ordered after it.
	epoch := d.issueClearEpoch(key)
	if _, leading := d.leads(); !leading {
		return "", fmt.Errorf("estop-clear: robot %q: %w", robotID, ErrNotLeading)
	}
	o := d.orderFor(key)
	var out clearResult
	var err error
	for attempt := 0; ; attempt++ {
		out, err = d.clearOnce(ctx, o, namespace, robotID, ends, epoch)
		if err == nil || !apierrors.IsConflict(err) || attempt == 9 {
			break
		}
		// Another write reached the Robot between the read and the patch: read it again and check
		// the guard again.
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	if err != nil {
		return out.state, fmt.Errorf("estop-clear: robot %q: %w", robotID, err)
	}
	if !out.written {
		return out.state, nil
	}
	// The baseline is taken once Resuming is written, so only a frame the robot sends after the
	// clear can leave the window.
	d.armResumeWindow(key, d.clock())
	// The work the robot records is held now, not when a FleetAction reconcile next reads the robot
	// Resuming: the release may come first (holdClearedWork). A failure here is logged; the release
	// holds the work again before it writes Normal, and refuses while it cannot.
	if err := d.holdClearedWork(ctx, out.robot); err != nil {
		log.FromContext(ctx).WithName("estop-dispatcher").Error(err, "holding the work of a cleared robot; the release holds it",
			"robot", robotID)
	}
	switch out.path {
	case clearSend:
		d.sendClears(out.robot, out.ended, clearedBy, out.clearedAt)
	case clearHold:
		log.FromContext(ctx).WithName("estop-dispatcher").Info("EstopClear not sent: the adapter's contract version "+
			"is not known to this process, or not supported; the entries are kept, and the adapter's report sends "+
			"their EstopClear (1.4.0) or removes them (1.3.x)", "robot", robotID, "stops", out.ended)
	}
	return fleetv1.RobotEstopResuming, nil
}

// clearResult is one clear's outcome: the state that stands, whether Resuming was written, and
// the stops it ended.
type clearResult struct {
	state     fleetv1.RobotEstopState
	written   bool
	robot     *fleetv1.Robot
	ended     []string
	clearedAt time.Time
	path      clearPath
}

// clearOnce is one read, guard and patch of ClearEstop; the guard and the patch run under the
// robot's write lock.
func (d *Dispatcher) clearOnce(ctx context.Context, o *estopOrder, namespace, robotID string, ends Ends,
	epoch uint64) (clearResult, error) {
	logger := log.FromContext(ctx).WithValues("robot", robotID, "namespace", namespace, "epoch", epoch)
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	// Read before the robot's write lock is taken, so a slow read delays no stop's outcome. A write
	// that lands after this read fails the clear's precondition, and the clear reads again.
	stored := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKey{Name: robotID, Namespace: namespace}, stored); err != nil {
		return clearResult{}, fmt.Errorf("lookup: %w", err)
	}
	o.write.Lock()
	defer o.write.Unlock()
	res := clearResult{state: stored.Status.EstopState, robot: stored}
	if res.state == "" {
		res.state = fleetv1.RobotEstopNormal
	}
	if d.supersededByLaterStop(o, epoch) {
		logger.Info("estop clear superseded by a trigger issued after it; the stop stands")
		return res, nil
	}
	switch res.state {
	case fleetv1.RobotEstopNormal:
		if len(stored.Status.EstopHeldBy) == 0 {
			return res, nil // nothing to clear
		}
	case fleetv1.RobotEstopStopping:
		// Row S5 moves only Stopped or Failed: the stop's outcome is not known yet, and a clear
		// does not move a Stopping robot. Its later answer is written as usual; only a fresh stop
		// and its clear then release it.
		logger.Info("estop clear does not move a Stopping robot; the stop's answer is awaited")
		return res, nil
	case fleetv1.RobotEstopResuming:
		return res, nil // already cleared
	}
	if _, own := stored.Annotations[estopscope.TriggerAnnotation]; own {
		logger.Info("estop clear not written: the robot carries its own trigger")
		return res, nil
	}
	var holds []fleetv1.EstopHold
	for _, h := range stored.Status.EstopHeldBy {
		if ends.Hold == nil || !ends.Hold(h) {
			holds = append(holds, h)
		}
	}
	if len(holds) > 0 {
		logger.Info("estop clear not written: another stop is recorded on the robot", "heldBy", holds[0].Name)
		return res, nil
	}
	res.path = d.adapterClearPath(stored)
	legacy := res.path == clearRemove
	// clearedAt is stored to the second; the EstopClear carries the value stored.
	res.clearedAt = d.clock().Truncate(time.Second)
	at := metav1.NewTime(res.clearedAt)
	recorded := map[string]bool{}
	var stops []fleetv1.RobotStop
	for _, st := range stored.Status.Stops {
		recorded[st.ID] = true
		if st.ClearedAt != nil {
			stops = append(stops, st)
			continue
		}
		if !stopEnded(st, stored, ends) {
			logger.Info("estop clear not written: a recorded stop is still in force", "stopID", st.ID, "scope", st.Scope)
			return res, nil
		}
		res.ended = append(res.ended, st.ID)
		if !legacy {
			st.ClearedAt = &at
			stops = append(stops, st)
		}
	}
	// A stop this process sent and has not recorded yet is in its own state: it is in force unless
	// its carrier has ended, and then this clear ends it too.
	var unrecorded []*stopRecord
	for _, r := range d.inflightOf(robotEstopKey(namespace, robotID)) {
		if recorded[r.entry.ID] || d.settledByClear(r) {
			continue
		}
		if !stopEnded(r.entry, stored, ends) {
			logger.Info("estop clear not written: a stop sent and not yet recorded is still in force", "stopID", r.entry.ID)
			return res, nil
		}
		unrecorded = append(unrecorded, r)
		res.ended = append(res.ended, r.entry.ID)
	}
	base := stored.DeepCopy()
	stored.Status.EstopState = fleetv1.RobotEstopResuming
	stored.Status.EstopHeldBy = nil
	stored.Status.Stops = stops
	// A record of an unrecorded stop this clear ends waits for this write (clearOf).
	d.mu.Lock()
	for _, r := range unrecorded {
		r.clearing = make(chan struct{})
	}
	d.mu.Unlock()
	err := d.Client.Status().Patch(ctx, stored, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	d.mu.Lock()
	for _, r := range unrecorded {
		if err == nil {
			t := res.clearedAt
			r.clearedAt, r.removed = &t, legacy
		}
		close(r.clearing)
		r.clearing = nil
	}
	d.mu.Unlock()
	if err != nil {
		if !apierrors.IsConflict(err) {
			logger.Error(err, "writing the clear")
		}
		return res, err
	}
	res.state, res.written = fleetv1.RobotEstopResuming, true
	o.stoppedBy = 0
	if legacy {
		// The 1.3.x path removed the entries it ended, and an unrecorded stop it ended is never
		// recorded: each ID is remembered, so a later report of it is answered (removed.go).
		gone := removedEntries(base.Status.Stops, stops)
		for _, r := range unrecorded {
			gone = append(gone, r.entry)
		}
		for i := range gone {
			gone[i].ClearedAt = &at
		}
		d.rememberRemoved(stored, gone)
	}
	d.mu.Lock()
	// The robot's stops in force end for its AssignActions now, unless a trigger was issued since
	// this clear's epoch: that stop's fence then stands, and so does every other until its clear.
	// It is done under d.mu, which a trigger takes to issue its epoch, so no later stop's fence is
	// ended here.
	if d.Fence != nil && o.lastStop < epoch {
		d.Fence.ClearHeld(namespace, robotID)
	}
	d.mu.Unlock()
	sort.Strings(res.ended)
	logger.Info("estop cleared to Resuming", "ended", res.ended, "removed", legacy)
	return res, nil
}

// stopEnded reports whether a stop recorded on robot (or sent and not yet recorded) has ended for
// this clear. A stop of the robot's own carries no trigger now (the clear reads the robot after
// the trigger is gone), and one recorded under another robot of the same name has ended with
// that robot. A zone's or the namespace's stop has ended when ends says so. A reported or an
// end-state stop is never ended by a clear.
func stopEnded(st fleetv1.RobotStop, robot *fleetv1.Robot, ends Ends) bool {
	switch {
	case st.Carrier == nil:
		return st.Scope == fleetv1.StopScopeRobot
	case st.Carrier.Kind == fleetv1.StopCarrierRobot:
		return true
	}
	return ends.Stop != nil && ends.Stop(st)
}

// sendClears sends one EstopClear per ended stop on the robot's SafetyStream, once the clear's
// write has landed, while this process leads, with the leader term. With no stream, or a send
// that fails, each entry awaits: its EstopClear goes again on the adapter's next SafetyStream
// (resendClears).
func (d *Dispatcher) sendClears(robot *fleetv1.Robot, ids []string, clearedBy string, at time.Time) {
	logger := log.Log.WithName("estop-dispatcher").WithValues("robot", robot.Name, "namespace", robot.Namespace)
	term, leading := d.leads()
	if !leading {
		logger.Info("EstopClear not sent: this process no longer leads; the next leader sends it on the adapter's next SafetyStream")
		return
	}
	d.mu.Lock()
	sender := d.streams[adapterKey(robot.Namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		logger.Info("EstopClear not sent: no SafetyStream to the adapter; it is sent when one opens", "stops", ids)
		return
	}
	for _, id := range ids {
		d.mu.Lock()
		d.idCounter++
		seq := d.idCounter
		d.mu.Unlock()
		msg := &fav1.ControlPlaneSafetyMessage{Seq: seq, RobotId: robotid.Wire(robot),
			Payload: &fav1.ControlPlaneSafetyMessage_EstopClear{EstopClear: &fav1.EstopClear{
				EstopId: id, ClearedBy: clearedBy, ClearedAtMs: at.UnixMilli(), LeaderTerm: term}}}
		if err := sender.Send(msg); err != nil {
			logger.Error(err, "sending an EstopClear; it is sent again on the adapter's next SafetyStream", "stopID", id)
			return
		}
	}
	logger.Info("EstopClear sent", "stops", ids)
}

// resendClears sends, on a SafetyStream that has just opened for robot's adapter, the EstopClear
// of each of robot's recorded stops that awaits its EstopClearAck. It sends no Estop and no fence
// for them. A connection below 1.4.0 has no EstopClear: those entries are removed instead. One whose
// version is not known here is sent nothing (clearHold); its report sends them (ContractReported).
func (d *Dispatcher) resendClears(robot *fleetv1.Robot, sender controlstream.SafetySender) {
	var ids []string
	byID := map[string]time.Time{}
	for _, st := range robot.Status.Stops {
		if st.ClearedAt != nil {
			ids = append(ids, st.ID)
			byID[st.ID] = st.ClearedAt.Time
		}
	}
	if len(ids) == 0 {
		return
	}
	switch d.adapterClearPath(robot) {
	case clearRemove:
		d.removeCleared(robot, func(fleetv1.RobotStop) bool { return true })
		return
	case clearHold:
		log.Log.WithName("estop-dispatcher").Info("EstopClear not sent on a new SafetyStream: the adapter's "+
			"contract version is not known to this process, or not supported", "robot", robot.Name, "stops", ids)
		return
	}
	term, leading := d.leads()
	if !leading {
		return
	}
	for _, id := range ids {
		d.mu.Lock()
		d.idCounter++
		seq := d.idCounter
		d.mu.Unlock()
		if err := sender.Send(&fav1.ControlPlaneSafetyMessage{Seq: seq, RobotId: robotid.Wire(robot),
			Payload: &fav1.ControlPlaneSafetyMessage_EstopClear{EstopClear: &fav1.EstopClear{
				EstopId: id, ClearedBy: heldStopIssuer, ClearedAtMs: byID[id].UnixMilli(), LeaderTerm: term}}}); err != nil {
			log.Log.WithName("estop-dispatcher").Error(err, "re-sending an EstopClear on a new SafetyStream",
				"robot", robot.Name, "stopID", id)
			return
		}
	}
	log.Log.WithName("estop-dispatcher").Info("EstopClear sent again on a new SafetyStream", "robot", robot.Name, "stops", ids)
}

// removeAwaitingClears removes, for every robot of identity's adapter, the recorded stops that
// await an EstopClearAck: the adapter reported a contract below 1.4.0, which has no EstopClear.
func (d *Dispatcher) removeAwaitingClears(identity controlstream.TLSIdentity) {
	if d.Client == nil {
		return
	}
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultConfirmTimeout)
	defer cancel()
	var robots fleetv1.RobotList
	if err := reader.List(ctx, &robots, client.InNamespace(identity.Namespace)); err != nil {
		log.Log.WithName("estop-dispatcher").Error(err, "listing robots to remove the cleared stops of a 1.3 adapter",
			"adapter", identity.AdapterName)
		return
	}
	for i := range robots.Items {
		if robots.Items[i].Spec.Adapter.Name == identity.AdapterName {
			d.removeCleared(&robots.Items[i], func(fleetv1.RobotStop) bool { return true })
		}
	}
}

// sendAwaitingClears sends, for every robot of identity's adapter, on that adapter's current
// SafetyStream, the EstopClear of each recorded stop that awaits its EstopClearAck (resendClears):
// the adapter reported 1.4.0, and a clear taken while its version was not known here sent none.
func (d *Dispatcher) sendAwaitingClears(identity controlstream.TLSIdentity) {
	if d.Client == nil {
		return
	}
	d.mu.Lock()
	sender := d.streams[adapterKey(identity.Namespace, identity.AdapterName)]
	d.mu.Unlock()
	if sender == nil {
		// The SafetyStream that opens next sends them (resendHeld).
		return
	}
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultConfirmTimeout)
	defer cancel()
	var robots fleetv1.RobotList
	if err := reader.List(ctx, &robots, client.InNamespace(identity.Namespace)); err != nil {
		log.Log.WithName("estop-dispatcher").Error(err, "listing robots to send the clears a 1.4 adapter awaits",
			"adapter", identity.AdapterName)
		return
	}
	for i := range robots.Items {
		if robots.Items[i].Spec.Adapter.Name == identity.AdapterName {
			d.resendClears(&robots.Items[i], sender)
		}
	}
}

// removeCleared removes from robot's status.stops each entry with a clearedAt that match selects,
// and remembers each ID it removed (removed.go).
func (d *Dispatcher) removeCleared(robot *fleetv1.Robot, match func(fleetv1.RobotStop) bool) {
	var gone []fleetv1.RobotStop
	var on *fleetv1.Robot
	landed := d.writeStopRecord(client.ObjectKeyFromObject(robot), "", func(r *fleetv1.Robot) (bool, bool) {
		var keep []fleetv1.RobotStop
		gone, on = nil, r
		for _, st := range r.Status.Stops {
			if st.ClearedAt == nil || !match(st) {
				keep = append(keep, st)
			} else {
				gone = append(gone, st)
			}
		}
		if len(keep) == len(r.Status.Stops) {
			return false, false
		}
		r.Status.Stops = keep
		return true, false
	})
	if landed && on != nil {
		d.rememberRemoved(on, gone)
	}
}

// RouteClearAck takes an EstopClearAck: the adapter lifted the stop an EstopClear named. robotID
// is the wire robot_id it names, already authorized against identity. It is taken only from the
// adapter's current SafetyStream: one from a stream that has been replaced changes nothing. It
// removes, in a goroutine of its own, the robot's entry with that ID and a clearedAt. An ack for an
// entry with no clearedAt, or for no entry, changes nothing and is logged. It never blocks the
// SafetyStream receive loop, and returns no error for a robot the stream may serve.
func (d *Dispatcher) RouteClearAck(identity controlstream.TLSIdentity, from controlstream.SafetySender, robotID string,
	ack *fav1.EstopClearAck) error {
	if ack == nil || !identity.Verified || robotID == "" {
		return nil
	}
	logger := log.Log.WithName("estop-dispatcher").WithValues("robot", robotID, "namespace", identity.Namespace,
		"stopID", ack.GetEstopId())
	d.mu.Lock()
	current := d.streams[adapterKey(identity.Namespace, identity.AdapterName)] == from
	d.mu.Unlock()
	if !current {
		logger.Info("EstopClearAck on a SafetyStream that has been replaced; it changes nothing")
		return nil
	}
	go func() {
		var reader client.Reader = d.Client
		if d.APIReader != nil {
			reader = d.APIReader
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultConfirmTimeout)
		defer cancel()
		robot, err := robotid.Lookup(ctx, reader, identity.Namespace, robotID)
		if err != nil || robot == nil {
			logger.Info("EstopClearAck for a robot that could not be read; it changes nothing", "err", fmt.Sprint(err))
			return
		}
		id := ack.GetEstopId()
		// A stop cleared before its record landed: the record is not written at all.
		if d.markUnrecordedAcked(robotEstopKey(robot.Namespace, robot.Name), id) {
			logger.Info("EstopClearAck: the cleared stop's record is not written")
			return
		}
		found := false
		var gone fleetv1.RobotStop
		var on *fleetv1.Robot
		landed := d.writeStopRecord(client.ObjectKeyFromObject(robot), id, func(r *fleetv1.Robot) (bool, bool) {
			found = false
			for i, st := range r.Status.Stops {
				if st.ID != id {
					continue
				}
				if st.ClearedAt == nil {
					return false, false
				}
				found, gone, on = true, st, r
				r.Status.Stops = append(r.Status.Stops[:i:i], r.Status.Stops[i+1:]...)
				return true, false
			}
			return false, false
		})
		if !found || !landed {
			logger.Info("EstopClearAck names no cleared stop of the robot; it changes nothing")
			return
		}
		d.rememberRemoved(on, []fleetv1.RobotStop{gone})
		logger.Info("recorded stop removed: the adapter lifted it")
	}()
	return nil
}

// trackInflight keeps r, a stop sent and not yet recorded, in this process's state until its
// record has landed or been given up.
func (d *Dispatcher) trackInflight(key string, r *stopRecord) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight == nil {
		d.inflight = map[string]map[string]*stopRecord{}
	}
	if d.inflight[key] == nil {
		d.inflight[key] = map[string]*stopRecord{}
	}
	d.inflight[key][r.entry.ID] = r
}

func (d *Dispatcher) untrackInflight(key string, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight[key], id)
	if len(d.inflight[key]) == 0 {
		delete(d.inflight, key)
	}
}

// inflightOf is the robot's stops sent and not yet recorded.
func (d *Dispatcher) inflightOf(key string) []*stopRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*stopRecord, 0, len(d.inflight[key]))
	for _, r := range d.inflight[key] {
		out = append(out, r)
	}
	return out
}

// settledByClear reports whether a clear has ended this unrecorded stop.
func (d *Dispatcher) settledByClear(r *stopRecord) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return r.clearedAt != nil
}

// markUnrecordedAcked marks an unrecorded stop that a clear ended as lifted, so its record is not
// written; it reports whether there was one.
func (d *Dispatcher) markUnrecordedAcked(key, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.inflight[key][id]
	if r == nil || r.clearedAt == nil {
		return false
	}
	r.removed = true
	return true
}

// SentUnrecorded is the robots in namespace (by object name) that this process has sent a stop of
// carrier, has not recorded yet, and whose stop no clear has ended. Such a stop is in this
// process's own state until its record lands (RFC-0001 #safety-estop-scopes), so the stop has
// reached the robot. A carrier matches by kind and name, and by UID when both name one.
func (d *Dispatcher) SentUnrecorded(namespace string, carrier Carrier) map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]bool{}
	for _, recs := range d.inflight {
		for _, r := range recs {
			c := r.entry.Carrier
			if r.robot.Namespace != namespace || r.clearedAt != nil || c == nil || c.Kind != carrier.Kind ||
				c.Name != carrier.Name || (carrier.UID != "" && c.UID != "" && c.UID != carrier.UID) {
				continue
			}
			out[r.robot.Name] = true
		}
	}
	return out
}

// unrecordedInForce reports whether this process sent the robot a stop it has not recorded yet
// and no clear has ended.
func (d *Dispatcher) unrecordedInForce(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.inflight[key] {
		if r.clearedAt == nil {
			return true
		}
	}
	return false
}
