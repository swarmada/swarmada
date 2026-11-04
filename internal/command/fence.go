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

package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A stop's fence and its order with AssignActions (RFC-0001 #safety-action-table, Rules for every
// A row; #fleet-adapter-protocol-the-stop-latch). The fence is the stop's Estop sent on the robot's
// ControlStream as Command.estop, in order with the AssignActions. The dispatcher keeps, per
// robot, the stop IDs in force, and one send lock covers two sections: the "no stop holds the
// robot" check with the AssignAction send, and the add of a stop ID to that set with the fence's
// send. So an AssignAction that passes the check is on the wire ahead of the fence of every later
// stop, and one that fails it is not sent at all.

// ErrHeldByStop means an AssignAction was not sent because a stop in force holds the robot.
// Nothing went on the wire: it is no delivery failure and no refusal.
var ErrHeldByStop = errors.New("assign_action not sent: a stop in force holds the robot")

// ErrNotRegistered means an AssignAction was not sent because the robot's register has not been
// processed on its adapter's current ControlStream, so the fences of the stops in force have not
// gone down that stream yet. Nothing went on the wire.
var ErrNotRegistered = errors.New("assign_action not sent: the robot's register has not been processed on its adapter's ControlStream")

// ErrBelowLatchContract means an AssignAction was not sent because its ControlStream did not
// negotiate contract 1.4.0, so the Stop Latch does not bind the adapter on it. Nothing went on the
// wire.
var ErrBelowLatchContract = errors.New("assign_action not sent: the adapter's ControlStream did not negotiate contract 1.4.0")

// ErrWithheldByStop means a zone_admission with admit = true, or a reservation_granted, was not
// sent because a stop holds the robot. It is kept, and sent once the robot is released (S6) if the
// hold or the queue it answers still stands (RobotReleased). Nothing went on the wire.
var ErrWithheldByStop = errors.New("not sent: a stop holds the robot; sent once the robot is released")

// fenceState is the dispatcher's stops-in-force and readiness bookkeeping, guarded by its own mu.
type fenceState struct {
	mu    sync.Mutex
	held  map[string]map[string]bool        // namespace/name/uid → the stop IDs in force
	locks map[string]*sync.Mutex            // namespace/name/uid → the robot's send lock
	ready map[CommandSender]map[string]bool // stream → wire robot_ids whose register it processed and fenced
	// registered is the robots whose register a stream processed, fenced or not: a robot whose
	// fences could not be sent then is fenced at its next AssignAction (roundTripAssign).
	registered map[CommandSender]map[string]bool
	gen        map[string]uint64            // namespace/name/uid → bumped each time a stop is added
	contract   map[CommandSender]string     // stream → the contract version agreed for it
	withheld   map[string]map[string]motion // namespace/name/uid → motion withheld by a stop, by key
}

func (f *fenceState) init() {
	if f.held == nil {
		f.held, f.locks, f.ready = map[string]map[string]bool{}, map[string]*sync.Mutex{}, map[CommandSender]map[string]bool{}
		f.registered, f.gen, f.withheld = map[CommandSender]map[string]bool{}, map[string]uint64{}, map[string]map[string]motion{}
		f.contract = map[CommandSender]string{}
	}
}

// robotKey keys a robot's stops in force and its send lock by namespace, name and UID: a robot
// deleted and created again under its name is another robot, which no stop of the first holds.
func robotKey(namespace, name string, uid types.UID) string {
	return namespace + "/" + name + "/" + string(uid)
}

// robotLock is the robot's send lock.
func (d *Dispatcher) robotLock(key string) *sync.Mutex {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	l := d.fence.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		d.fence.locks[key] = l
	}
	return l
}

// heldStops is the sorted stop IDs in force on the robot.
func (d *Dispatcher) heldStops(key string) []string {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	var out []string
	for id := range d.fence.held[key] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (d *Dispatcher) addHeld(key, stopID string) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	if d.fence.held[key] == nil {
		d.fence.held[key] = map[string]bool{}
	}
	d.fence.held[key][stopID] = true
	d.fence.gen[key]++
}

// stopGen is the count of stops added to the robot's stops in force so far.
func (d *Dispatcher) stopGen(key string) uint64 {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	return d.fence.gen[key]
}

// ClearHeld empties the robot's stops in force, whatever its UID. The estop dispatcher calls it
// once the clear that returns the robot to Resuming has been written: from then on the robot may
// be given work again, through the operator's resume and the scheduler's checks.
func (d *Dispatcher) ClearHeld(namespace, robot string) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	prefix := namespace + "/" + robot + "/"
	for k := range d.fence.held {
		if strings.HasPrefix(k, prefix) {
			delete(d.fence.held, k)
		}
	}
}

// HeldStops is the stop IDs in force on the robot named robot, whatever its UID, sorted. The
// estop dispatcher's release (S6) reads it: no robot is released while one is in force.
func (d *Dispatcher) HeldStops(namespace, robot string) []string {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	prefix := namespace + "/" + robot + "/"
	var out []string
	for k, ids := range d.fence.held {
		if strings.HasPrefix(k, prefix) {
			for id := range ids {
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (d *Dispatcher) markReady(sender CommandSender, wireID string) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	if d.fence.ready[sender] == nil {
		d.fence.ready[sender] = map[string]bool{}
	}
	d.fence.ready[sender][wireID] = true
}

func (d *Dispatcher) isReady(sender CommandSender, wireID string) bool {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	return d.fence.ready[sender][wireID]
}

func (d *Dispatcher) markRegistered(sender CommandSender, wireID string) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	if d.fence.registered[sender] == nil {
		d.fence.registered[sender] = map[string]bool{}
	}
	d.fence.registered[sender][wireID] = true
}

func (d *Dispatcher) isRegistered(sender CommandSender, wireID string) bool {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	return d.fence.registered[sender][wireID]
}

// StreamNegotiated records the contract version agreed for sender's ControlStream
// (controlstream.StreamContractObserver). The server calls it once the stream is registered, before
// it reads any message from it, so before any register there can make a robot ready for work. A
// stream that is not registered here (an unverified identity) is ignored: nothing is sent on it, and
// nothing would forget it.
func (d *Dispatcher) StreamNegotiated(sender CommandSender, version string) {
	d.mu.Lock()
	live := false
	for _, s := range d.streams {
		if s == sender {
			live = true
			break
		}
	}
	d.mu.Unlock()
	if !live {
		return
	}
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	d.fence.contract[sender] = version
}

// latchContractMin is the contract version whose connections the Stop Latch binds.
const latchContractMin = "1.4.0"

// boundByLatch reports whether sender's ControlStream negotiated contract 1.4.0. A stream whose
// version was never recorded, or cannot be read, is not known to be bound: fail closed.
func (d *Dispatcher) boundByLatch(sender CommandSender) bool {
	d.fence.mu.Lock()
	v, told := d.fence.contract[sender]
	d.fence.mu.Unlock()
	below, ok := contract.Below(v, latchContractMin)
	return told && ok && !below
}

func (d *Dispatcher) forgetStream(sender CommandSender) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	delete(d.fence.ready, sender)
	delete(d.fence.registered, sender)
	delete(d.fence.contract, sender)
}

// SendFence sends a stop's fence, its Estop as Command.estop, on robot's ControlStream, after
// adding the stop's ID to the robot's stops in force, both under the robot's send lock. It goes
// through the one Command send site, so it carries the leader term and is not sent by a process
// that does not lead. It waits for no answer: the CommandResult is a receipt that no stop row
// reads, logged only if it says stale_leader_term, which an adapter never answers a stop with. With
// no ControlStream the stop stays in force here, and the fence goes down the robot's next stream
// when its register is processed (RobotRegistered).
func (d *Dispatcher) SendFence(robot *fleetv1.Robot, stopID string, estop *fav1.Estop) error {
	key := robotKey(robot.Namespace, robot.Name, robot.UID)
	lock := d.robotLock(key)
	lock.Lock()
	defer lock.Unlock()
	d.addHeld(key, stopID)
	d.mu.Lock()
	sender := d.streams[adapterKey(robot.Namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		return fmt.Errorf("%w: no ControlStream to adapter %q for the fence of stop %s", ErrUnreachable, robot.Spec.Adapter.Name, stopID)
	}
	return d.sendFenceOn(sender, robotid.Wire(robot), robot.Name, stopID, estop)
}

// SendClearedFence sends a cleared stop's fence, its Estop as Command.estop, on robot's ControlStream
// under the robot's send lock, and adds nothing to the robot's stops in force: it answers the
// adapter's report of a stop the control plane already cleared (RFC-0001 #safety-robot-stop-table,
// Reported stop IDs and local-latch reports), so that stop's EstopClear can take effect at the
// adapter. With no ControlStream nothing is sent; the next report is answered again.
func (d *Dispatcher) SendClearedFence(robot *fleetv1.Robot, stopID string, estop *fav1.Estop) error {
	key := robotKey(robot.Namespace, robot.Name, robot.UID)
	lock := d.robotLock(key)
	lock.Lock()
	defer lock.Unlock()
	d.mu.Lock()
	sender := d.streams[adapterKey(robot.Namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		return fmt.Errorf("%w: no ControlStream to adapter %q for the fence of cleared stop %s", ErrUnreachable,
			robot.Spec.Adapter.Name, stopID)
	}
	return d.sendFenceOn(sender, robotid.Wire(robot), robot.Name, stopID, estop)
}

// sendFenceOn sends the fence of stopID down sender and watches for its receipt. The caller holds
// the robot's send lock.
func (d *Dispatcher) sendFenceOn(sender CommandSender, wireID, robot, stopID string, estop *fav1.Estop) error {
	fence := proto.Clone(estop).(*fav1.Estop)
	fence.EstopId = stopID
	id, ch := d.register(sender, wireID)
	cmd := &fav1.Command{RobotId: wireID, CommandId: id, Command: &fav1.Command_Estop{Estop: fence}}
	if err := d.send(sender, cmd); err != nil {
		d.deregister(id)
		return err
	}
	go func() {
		defer d.deregister(id)
		select {
		case res := <-ch:
			if res.GetStaleLeaderTerm() {
				log.Log.WithName("command-dispatcher").Error(nil, "an adapter rejected a stop's fence for its leader term; "+
					"an adapter never rejects a stop, so this is an adapter defect", "robot", robot, "stopID", stopID,
					"commandID", id, "leaderTerm", cmd.GetLeaderTerm())
			}
		case <-time.After(d.timeout):
		}
	}()
	return nil
}

// roundTripAssign is roundTrip for an AssignAction. Under the robot's send lock it checks that
// no stop in force holds the robot and, when RequireRegister is set, that the robot's register
// has been processed on the stream, and then sends; the wait for the result is outside the lock.
// A check that fails sends nothing: ErrHeldByStop or ErrNotRegistered.
func (d *Dispatcher) roundTripAssign(ctx context.Context, namespace, robotName string, cmd *fav1.Command) (*fav1.CommandResult, error) {
	robot := &fleetv1.Robot{}
	if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotName}, robot); err != nil {
		return nil, fmt.Errorf("command: robot %q lookup: %w", robotName, err)
	}
	wireID := robotid.Wire(robot)
	key := robotKey(namespace, robotName, robot.UID)
	lock := d.robotLock(key)
	lock.Lock()
	d.mu.Lock()
	sender := d.streams[adapterKey(namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		lock.Unlock()
		return nil, fmt.Errorf("%w: no ControlStream to adapter %q", ErrUnreachable, robot.Spec.Adapter.Name)
	}
	if d.RequireContract && !d.boundByLatch(sender) {
		lock.Unlock()
		return nil, fmt.Errorf("%w: adapter %q", ErrBelowLatchContract, robot.Spec.Adapter.Name)
	}
	if stops := d.heldStops(key); len(stops) > 0 {
		lock.Unlock()
		return nil, fmt.Errorf("%w (%s)", ErrHeldByStop, strings.Join(stops, ", "))
	}
	if d.RequireRegister && !d.isReady(sender, wireID) {
		// The register was processed on this stream and its fences were not sent then (the
		// Robot could not be read, or a send failed): they are sent now, first, under this lock.
		if !d.isRegistered(sender, wireID) {
			lock.Unlock()
			return nil, ErrNotRegistered
		}
		if err := d.fenceOnRegister(sender, wireID, robot, key); err != nil {
			lock.Unlock()
			return nil, err
		}
		if stops := d.heldStops(key); len(stops) > 0 {
			lock.Unlock()
			return nil, fmt.Errorf("%w (%s)", ErrHeldByStop, strings.Join(stops, ", "))
		}
	}
	id, ch := d.register(sender, wireID)
	defer d.deregister(id)
	cmd.RobotId = wireID
	cmd.CommandId = id
	err := d.send(sender, cmd)
	lock.Unlock()
	if err != nil {
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

// RobotRegistered is called by the ControlStream server once it has processed a robot's register
// on sender and sent its accepting RegisterAck, while this process leads. Under the robot's send
// lock it sends down sender the fence of every stop in force on the robot, then marks the robot
// ready on that stream, so no AssignAction reaches the robot on a new ControlStream before those
// fences (RFC-0001 #safety-robot-stop-table, Sending a stop again). The stops in force are the
// dispatcher's own and the robot's recorded stops with no clearedAt while the robot is in a stop
// state (Stopping, Stopped or Failed).
//
// The register is recorded first. The Robot is read a few times, past the cache when a reader is
// wired (a robot admitted a moment ago may not be in the cache yet). If it still cannot be read,
// or a fence cannot be sent, the robot is not marked ready, and its next AssignAction on this
// stream sends the fences first (roundTripAssign): a failed read does not leave the robot unready
// for the life of the stream. A fence that cannot be sent is a Warning Event on the Robot.
func (d *Dispatcher) RobotRegistered(ctx context.Context, sender CommandSender, namespace, wireID string) {
	logger := log.FromContext(ctx).WithValues("namespace", namespace, "robot", wireID)
	d.markRegistered(sender, wireID)
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	var robot *fleetv1.Robot
	var err error
	backoff := 50 * time.Millisecond
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		if robot, err = robotid.Lookup(ctx, reader, namespace, wireID); err == nil && robot != nil {
			break
		}
	}
	if err != nil || robot == nil {
		logger.Error(err, "robot not marked ready on its ControlStream: it could not be read; its fences go "+
			"ahead of its next AssignAction on this stream")
		return
	}
	key := robotKey(namespace, robot.Name, robot.UID)
	lock := d.robotLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := d.fenceOnRegister(sender, wireID, robot, key); err != nil {
		logger.Error(err, "sending a stop's fence on a new ControlStream; the robot is not marked ready")
	}
}

// fenceOnRegister sends down sender the fence of every stop in force on robot and marks it ready
// there. The caller holds the robot's send lock. A fence that cannot be sent leaves the robot not
// ready, raises a Warning Event on the Robot, and is returned.
func (d *Dispatcher) fenceOnRegister(sender CommandSender, wireID string, robot *fleetv1.Robot, key string) error {
	switch robot.Status.EstopState {
	case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed:
		for _, st := range robot.Status.Stops {
			if st.ClearedAt == nil {
				d.addHeld(key, st.ID)
			}
		}
	}
	for _, id := range d.heldStops(key) {
		estop := &fav1.Estop{Reason: "the stop in force, sent again as its fence on a new ControlStream",
			IssuedBy: "swarmada-control-plane", IssuedAtMs: d.clock().UnixMilli()}
		if err := d.sendFenceOn(sender, wireID, robot.Name, id, estop); err != nil {
			if d.Recorder != nil {
				d.Recorder.Eventf(robot, corev1.EventTypeWarning, "FencesNotSent",
					"the fence of stop %s could not be sent on the adapter's ControlStream (%v); no AssignAction "+
						"is sent to this robot on that stream until the fences are", id, err)
			}
			return fmt.Errorf("sending the fence of stop %s: %w", id, err)
		}
	}
	d.markReady(sender, wireID)
	return nil
}

// motion is a Command that lets a robot move on, withheld while a stop holds it: a zone_admission
// with admit = true for a zone, or a reservation_granted for a resource under the wire robot_id
// the grant names.
type motion struct {
	zone     string
	resource string
	robotID  string
}

func (m motion) key() string {
	if m.zone != "" {
		return "zone/" + m.zone
	}
	return "resource/" + m.resource
}

// heldByStop says why a stop holds robot, read from the Robot: a stop state, Resuming (cleared and
// not yet released), its own trigger or a zone's or the namespace's recorded stop.
func heldByStop(robot *fleetv1.Robot) (string, bool) {
	switch st := robot.Status.EstopState; st {
	case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed, fleetv1.RobotEstopResuming:
		return "estopState " + string(st), true
	}
	if by, ok := estopscope.HeldOnRecord(robot); ok {
		return by.String(), true
	}
	return "", false
}

// roundTripMotion is roundTrip for a motion Command (RFC-0001 #safety-how-the-two-interact, I6: the
// control plane sends a robot a stop holds no zone_admission with admit = true and no
// reservation_granted). Under the robot's send lock it checks that no stop in force holds the robot
// (the stops this process sent, in the fence's order, as for an AssignAction) and that the Robot,
// read past the cache, is in no stop state, not Resuming and records no stop. A check that fails
// sends nothing, keeps the Command for RobotReleased and returns ErrWithheldByStop.
//
// The Robot is read before the lock is taken, so the read never delays a stop's fence. A stop added
// between the read and the lock is seen by the count of stops added (stopGen), and the Robot is read
// again: a stop and its clear between the two cannot let an admit through on a stale Normal.
func (d *Dispatcher) roundTripMotion(ctx context.Context, robot *fleetv1.Robot, m motion, cmd *fav1.Command) (*fav1.CommandResult, error) {
	return d.sendMotion(ctx, robot, m, cmd, false)
}

// sendMotion is roundTripMotion; released says it is the release's send of a withheld Command,
// which goes only while that Command is still withheld (a hold since has dropped it).
func (d *Dispatcher) sendMotion(ctx context.Context, robot *fleetv1.Robot, m motion, cmd *fav1.Command, released bool) (*fav1.CommandResult, error) {
	key := robotKey(robot.Namespace, robot.Name, robot.UID)
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	for attempt := 0; ; attempt++ {
		gen := d.stopGen(key)
		cur := &fleetv1.Robot{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(robot), cur); err != nil {
			return nil, fmt.Errorf("command: robot %q lookup: %w", robot.Name, err)
		}
		res, err := d.sendUnderLock(ctx, robot, cmd, func() error {
			if released && !d.takeWithheld(key, m) {
				return errNotWithheld
			}
			stops := d.heldStops(key)
			why, held := heldByStop(cur)
			switch {
			case d.stopGen(key) != gen && attempt < 8:
				if released {
					d.withhold(key, m)
				}
				return errReread
			case len(stops) > 0:
				why, held = "stops in force "+strings.Join(stops, ", "), true
			case d.stopGen(key) != gen:
				why, held = "stops keep arriving", true
			}
			if held {
				d.withhold(key, m)
				return fmt.Errorf("%w (%s)", ErrWithheldByStop, why)
			}
			return nil
		})
		if errors.Is(err, errReread) {
			continue
		}
		return res, err
	}
}

// roundTripHold sends a zone_admission with admit = false, never withheld, and drops the admit
// withheld for that zone under the same lock, so a release cannot send it after this hold.
func (d *Dispatcher) roundTripHold(ctx context.Context, robot *fleetv1.Robot, m motion, cmd *fav1.Command) (*fav1.CommandResult, error) {
	key := robotKey(robot.Namespace, robot.Name, robot.UID)
	return d.sendUnderLock(ctx, robot, cmd, func() error {
		d.takeWithheld(key, m)
		return nil
	})
}

var (
	errReread      = errors.New("a stop was added while the Robot was read; read it again")
	errNotWithheld = errors.New("no longer withheld: a hold since dropped it, or it was sent")
)

// sendUnderLock sends cmd to robot's adapter under the robot's send lock, once check has passed
// under it, and waits for the result outside the lock. A check that fails sends nothing.
func (d *Dispatcher) sendUnderLock(ctx context.Context, robot *fleetv1.Robot, cmd *fav1.Command, check func() error) (*fav1.CommandResult, error) {
	wireID := robotid.Wire(robot)
	lock := d.robotLock(robotKey(robot.Namespace, robot.Name, robot.UID))
	lock.Lock()
	if err := check(); err != nil {
		lock.Unlock()
		return nil, err
	}
	d.mu.Lock()
	sender := d.streams[adapterKey(robot.Namespace, robot.Spec.Adapter.Name)]
	d.mu.Unlock()
	if sender == nil {
		lock.Unlock()
		return nil, fmt.Errorf("%w: no ControlStream to adapter %q", ErrUnreachable, robot.Spec.Adapter.Name)
	}
	id, ch := d.register(sender, wireID)
	defer d.deregister(id)
	cmd.RobotId = wireID
	cmd.CommandId = id
	err := d.send(sender, cmd)
	lock.Unlock()
	if err != nil {
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

// withhold keeps m for the robot's release; a later Command for the same zone or resource
// replaces it.
func (d *Dispatcher) withhold(key string, m motion) {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	d.fence.init()
	if d.fence.withheld[key] == nil {
		d.fence.withheld[key] = map[string]motion{}
	}
	d.fence.withheld[key][m.key()] = m
}

// takeWithheld removes m from the robot's withheld Commands, and reports whether it was there.
func (d *Dispatcher) takeWithheld(key string, m motion) bool {
	d.fence.mu.Lock()
	defer d.fence.mu.Unlock()
	if _, ok := d.fence.withheld[key][m.key()]; !ok {
		return false
	}
	delete(d.fence.withheld[key], m.key())
	return true
}

// RobotReleased sends what a stop withheld from the robot, now that the operator release (S6) has
// returned it to Normal; the estop dispatcher calls it once that write has landed. Each Command
// goes through the same gate again, so one a new stop holds is withheld again. An admit goes while
// no hold for its zone has dropped it; a grant only while the promotion it answers still stands in
// FleetZone.status. It returns at once and sends from a goroutine of its own.
//
// The withheld Commands are in this process's memory, as the Zone Controller's own hold marker is.
// After a change of leader neither survives, and a robot held at a boundary waits for the next
// admission transition: a delay, not a motion.
func (d *Dispatcher) RobotReleased(namespace, robot string) {
	d.fence.mu.Lock()
	var todo []motion
	var key string
	prefix := namespace + "/" + robot + "/"
	for k, ms := range d.fence.withheld {
		if strings.HasPrefix(k, prefix) {
			key = k
			for _, m := range ms {
				todo = append(todo, m)
			}
		}
	}
	d.fence.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].key() < todo[j].key() })
	go func() {
		logger := log.Log.WithName("command-dispatcher").WithValues("namespace", namespace, "robot", robot)
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()
		r := &fleetv1.Robot{}
		if err := d.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robot}, r); err != nil ||
			robotKey(namespace, robot, r.UID) != key {
			logger.Info("withheld commands not sent after the release: the robot could not be read, or is another robot now",
				"err", fmt.Sprint(err))
			return
		}
		for _, m := range todo {
			var cmd *fav1.Command
			if m.zone != "" {
				cmd = &fav1.Command{Command: &fav1.Command_ZoneAdmission{
					ZoneAdmission: &fav1.ZoneAdmission{ZoneName: m.zone, Admit: true}}}
			} else {
				if !d.promotionStands(ctx, namespace, m) {
					d.takeWithheld(key, m)
					logger.Info("withheld reservation_granted dropped after the release: the promotion no longer stands",
						"resource", m.resource)
					continue
				}
				cmd = &fav1.Command{Command: &fav1.Command_ReservationGranted{
					ReservationGranted: &fav1.ReservationGranted{ResourceName: m.resource}}}
			}
			if _, err := d.sendMotion(ctx, r, m, cmd, true); err != nil && !errors.Is(err, errNotWithheld) {
				logger.Info("withheld command not delivered after the release", "command", m.key(), "err", err.Error())
			}
		}
	}()
}

// promotionStands reports whether robot m.robotID still holds m.resource through an
// adapter-initiated entry in some FleetZone's status, where the TDE persists the promotion a
// grant answers.
func (d *Dispatcher) promotionStands(ctx context.Context, namespace string, m motion) bool {
	var zones fleetv1.FleetZoneList
	if err := d.Client.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return false
	}
	for _, z := range zones.Items {
		for _, q := range z.Status.SharedResourceQueues {
			if q.ResourceName != m.resource {
				continue
			}
			for _, h := range q.CurrentHolders {
				if h.RobotID == m.robotID && h.ActionID == m.robotID {
					return true
				}
			}
		}
	}
	return false
}
