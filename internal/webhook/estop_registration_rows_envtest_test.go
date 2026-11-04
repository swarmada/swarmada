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

package webhook

import (
	"strings"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot that registers again while a stop holds its action is told IDLE, never
// IN_PROGRESS or REVOKING: the action stays held, and the robot is not told to carry on
// under the lease it holds (RFC-0001 #safety-action-table, rows A6 and
// A13). Each row reads the RegisterAck the registrar answers the reconnecting adapter.

// wantAckPhase requires the reconnect answer for robot to carry phase.
func (r *rig) wantAckPhase(robot, phase string) {
	r.t.Helper()
	ack := r.reconnectAck(robot)
	r.t.Logf("RESULT reconnect RegisterAck for %s: %s", robot, ack)
	if !strings.Contains(ack, "action_state="+phase+" ") {
		r.t.Errorf("INVARIANT %s: reconnect answered %s, want %s", robot, ack, phase)
	}
}

func registrationRows() []invariantRow {
	var rows []invariantRow

	// A Revoking action whose robot a stop holds, at each scope and outcome.
	for _, c := range []struct{ scope, outcome string }{
		{"robot", "stopped"}, {"robot", "failed"}, {"zone", "stopped"}, {"namespace", "failed"},
	} {
		c := c
		rows = append(rows, invariantRow{name: "registration/held-revoking-told-idle/" + c.scope + "/" + c.outcome, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			if c.scope == "namespace" {
				r.config()
			}
			r.robot("r1", "z")
			r.holdUnpinned(wl, "a-held", "r1")
			r.revoke(wl, "a-held", "r1")
			if c.outcome == "failed" {
				r.setMode("r1", ackNone)
			}
			r.trigger(r.stopCarrier(c.scope), "stop")
			r.waitStopped("r1")
			holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false) // the adapter is back
			r.wantAckPhase("r1", "ROBOT_ACTION_PHASE_IDLE")
			r.assertRevokingHeld(wl, "a-held", "r1")
		}})
	}

	// The zone's stop is on record but its outcome was never written: estopState reads
	// Normal, and the robot is held all the same.
	rows = append(rows, invariantRow{name: "registration/held-revoking-told-idle/zone/outcome-unwritten", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		r.setMode("r1", ackNone)
		rule := r.rf.add(refuseState("r1", fleetv1.RobotEstopFailed, -1))
		r.trigger(r.zoneObj("z"), "stop")
		if ok, last := r.waitFor("r1 records the zone's stop", 15*time.Second, func() (bool, string) {
			return len(r.get("r1").Status.EstopHeldBy) > 0 && r.rf.hitsOf(rule) > 0, holdsOf(r.get("r1").Status.EstopHeldBy)
		}); !ok {
			t.Fatalf("setup: r1 never recorded the zone's stop with its outcome refused (last %s)", last)
		}
		if s := r.get("r1").Status.EstopState; s != "" && s != fleetv1.RobotEstopNormal {
			t.Fatalf("setup: r1 reads %s; this row needs the stop held on record only", s)
		}
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		r.wantAckPhase("r1", "ROBOT_ACTION_PHASE_IDLE")
		r.assertRevokingHeld(wl, "a-held", "r1")
	}})

	// Resuming: the stop is cleared, the operator's resume not yet confirmed. The action is
	// still held, and the robot is still told IDLE.
	rows = append(rows, invariantRow{name: "registration/held-revoking-told-idle/robot/resuming", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		r.trigger(r.robotObj("r1"), "stop")
		r.waitStopped("r1")
		r.clearStop(r.robotObj("r1"))
		r.waitState("r1", fleetv1.RobotEstopResuming)
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		r.wantAckPhase("r1", "ROBOT_ACTION_PHASE_IDLE")
		r.assertRevokingHeld(wl, "a-held", "r1")
	}})

	// Control: with no stop, a Revoking action is still told REVOKING on reconnect.
	rows = append(rows, invariantRow{name: "registration/control-no-stop-told-revoking", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		r.wantAckPhase("r1", "ROBOT_ACTION_PHASE_REVOKING")
	}})

	return rows
}
