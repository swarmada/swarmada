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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A stop a predecessor recorded (RFC-0001 #safety-robot-stop-table, row S4): "Each stop's windows
// are its own. They run from its persisted sentAt (S1) and acknowledgedAt (S2) in status.stops, not
// from a timer in one replica. ... A leader that takes over runs out a window its predecessor
// recorded, and writes Failed itself." And: "A re-send's delivery window runs from that re-send. No
// re-send restarts the confirmation window."
//
// A trigger whose outcome a predecessor did not write is issued again by its controller after a
// change of leader: the processed marker is written only with the outcome. The stop it would issue
// is the one the predecessor recorded on the robot, so TriggerEstop runs that stop out instead of
// issuing a fresh one with fresh windows. A stop this process issued is its own, and a trigger that
// fires again for it issues a stop as before.

// markIssued records that this process issued the stop stopID, so no later trigger takes it for a
// predecessor's.
func (d *Dispatcher) markIssued(stopID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.issued == nil {
		d.issued = map[string]struct{}{}
	}
	d.issued[stopID] = struct{}{}
}

// predecessorStop is the stop of carrier's on robot that a predecessor recorded: recorded in
// status.stops, in force (no clearedAt), and neither issued nor awaited by this process.
func (d *Dispatcher) predecessorStop(robot *fleetv1.Robot, scope metrics.EstopScope, carrier Carrier) (fleetv1.RobotStop, bool) {
	want := d.newStopRecord(robot, "", scope, carrier, time.Time{}).entry
	if want.Carrier == nil {
		return fleetv1.RobotStop{}, false
	}
	wireID := robotid.Wire(robot)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, st := range robot.Status.Stops {
		if st.ClearedAt != nil || st.Scope != want.Scope || st.Carrier == nil ||
			st.Carrier.Kind != want.Carrier.Kind || st.Carrier.Name != want.Carrier.Name ||
			(st.Carrier.UID != "" && want.Carrier.UID != "" && st.Carrier.UID != want.Carrier.UID) {
			continue
		}
		if _, own := d.issued[st.ID]; own {
			continue
		}
		if p := d.pending[pendingKey{namespace: robot.Namespace, robot: wireID, estopID: st.ID}]; p != nil && p.epoch != 0 {
			// Another trigger of this process is running it out already. A stream-open re-send's
			// waiter (epoch 0) is taken over (register).
			continue
		}
		return st, true
	}
	return fleetv1.RobotStop{}, false
}

// runOutRecorded runs out the windows of st, a stop a predecessor recorded on robot, and writes its
// outcome. With an acknowledgedAt whose confirmation window has ended, on a robot that is not
// Stopped, it writes Failed at once. Either way it then sends the stop again under its recorded ID,
// with its fence, for a stop in force with no confirmed outcome is sent again, and writes the answer
// as a first answer: a STOPPED confirms rest, over the Failed too (S3). The delivery window runs
// from this re-send, and the confirmation window ends at acknowledgedAt + the confirmation timeout
// when acknowledgedAt is set. A Stopped robot is not failed at once: this leader holds no record of
// which stop's Stopped is stored, and the answer decides.
func (d *Dispatcher) runOutRecorded(ctx context.Context, robot *fleetv1.Robot, st fleetv1.RobotStop, reason,
	issuedBy string, scope metrics.EstopScope, epoch uint64) (Result, error) {
	logger := log.FromContext(ctx).WithName("estop-dispatcher").WithValues("robot", robot.Name,
		"namespace", robot.Namespace, "stopID", st.ID)
	var acked *time.Time
	if st.AcknowledgedAt != nil {
		t := st.AcknowledgedAt.Time
		acked = &t
	}
	if acked != nil && robot.Status.EstopState != fleetv1.RobotEstopStopped &&
		!d.clock().Before(acked.Add(d.confirmTimeout)) {
		logger.Info("a predecessor's stop ran out its recorded confirmation window: Failed; it is sent again")
		if err := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed,
			"no STOPPED confirmation within the confirm window its predecessor recorded", epoch); err != nil {
			return Result{Delivered: true, State: fleetv1.RobotEstopFailed}, err
		}
	}
	estop := &fav1.Estop{Reason: reason, IssuedBy: issuedBy, IssuedAtMs: st.SentAt.UnixMilli()}
	sender, p, seq := d.register(robot.Namespace, robot.Name, robotid.Wire(robot), robot.Spec.Adapter.Name, st.ID,
		epoch, estop)
	d.sendFence(robot, st.ID, estop)
	if sender == nil {
		werr := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "no SafetyStream to adapter "+robot.Spec.Adapter.Name, epoch)
		return Result{State: fleetv1.RobotEstopFailed}, joinUnwritten(ErrUndelivered, werr)
	}
	defer d.deregister(p)
	resentAt, resentWall := d.clock(), time.Now()
	if err := sender.Send(estopMessage(p.key.robot, seq, p.estop, p.key.estopID)); err != nil && d.settleUnlessResent(p, sender) {
		werr := d.recordOutcome(ctx, robot, fleetv1.RobotEstopFailed, "estop send failed: "+err.Error(), epoch)
		return Result{State: fleetv1.RobotEstopFailed}, joinUnwritten(fmt.Errorf("estop send: %w", err), werr)
	}
	logger.Info("a predecessor's stop sent again under its recorded ID; its recorded windows are run out")
	key := client.ObjectKeyFromObject(robot)
	return d.await(ctx, robot, p, scope, epoch, resentAt, resentWall, acked, func(at time.Time) {
		go d.recordAcknowledged(key.Namespace, key.Name, st.ID, at)
	})
}

// recordAcknowledged writes at as the acknowledgedAt of the robot's recorded stop id, unless it has
// one (row S2: the first STOPPING, once).
func (d *Dispatcher) recordAcknowledged(namespace, name, id string, at time.Time) {
	d.writeStopRecord(client.ObjectKey{Namespace: namespace, Name: name}, id, func(robot *fleetv1.Robot) (bool, bool) {
		for i := range robot.Status.Stops {
			if s := &robot.Status.Stops[i]; s.ID == id {
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
