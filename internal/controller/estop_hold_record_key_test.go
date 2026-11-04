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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A zone's or the namespace's stop reaches each robot as that robot's own stop (RFC-0001
// #safety-robot-stop-table, row S1). Which robots a stop in force must still be sent to is
// keyed on the stop's record on the robot, not on its stop state: a robot already stopped by
// another stop is sent this one, and a robot whose record of this stop has landed is sent
// nothing again, however often the pass runs.
func TestInForceTargets_KeysOnTheStopRecord(t *testing.T) {
	ref := holdRef{kind: fleetv1.EstopHoldFleetZone, name: "z", uid: "z-uid"}
	hold := []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z", UID: "z-uid"}}
	rec := func(kind fleetv1.StopCarrierKind, name string, cleared bool) []fleetv1.RobotStop {
		s := fleetv1.RobotStop{ID: "id-" + name, Scope: fleetv1.StopScopeZone, SentAt: metav1.Now(),
			Carrier: &fleetv1.StopCarrier{Kind: kind, Name: name, UID: "z-uid"}}
		if cleared {
			now := metav1.Now()
			s.ClearedAt = &now
		}
		return []fleetv1.RobotStop{s}
	}
	robot := func(name string, st fleetv1.RobotEstopState, held []fleetv1.EstopHold, stops []fleetv1.RobotStop) fleetv1.Robot {
		r := fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name}}
		r.Status.EstopState, r.Status.EstopHeldBy, r.Status.Stops = st, held, stops
		return r
	}
	all := []fleetv1.Robot{
		robot("reached", fleetv1.RobotEstopStopped, hold, rec(fleetv1.StopCarrierFleetZone, "z", false)),
		robot("recorded-no-hold", fleetv1.RobotEstopStopped, nil, rec(fleetv1.StopCarrierFleetZone, "z", false)),
		robot("stopped-by-another", fleetv1.RobotEstopStopped, nil, rec(fleetv1.StopCarrierRobot, "stopped-by-another", false)),
		robot("held-no-record", fleetv1.RobotEstopFailed, hold, nil),
		robot("record-cleared", fleetv1.RobotEstopStopped, hold, rec(fleetv1.StopCarrierFleetZone, "z", true)),
		robot("arrived", "", nil, nil),
		robot("recorded-but-normal", fleetv1.RobotEstopNormal, hold, rec(fleetv1.StopCarrierFleetZone, "z", false)),
	}
	stop, recordOnly := inForceTargets(ref, all, all, nil)
	names := func(rs []fleetv1.Robot) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.Name] = true
		}
		return m
	}
	gotStop, gotRecord := names(stop), names(recordOnly)
	for _, n := range []string{"stopped-by-another", "held-no-record", "record-cleared", "arrived", "recorded-but-normal"} {
		if !gotStop[n] {
			t.Errorf("%s is not sent the stop: %v", n, gotStop)
		}
	}
	for _, n := range []string{"reached", "recorded-no-hold"} {
		if gotStop[n] {
			t.Errorf("%s, whose record of the stop has landed, is sent it again", n)
		}
	}
	if !gotRecord["recorded-no-hold"] || len(gotRecord) != 1 {
		t.Errorf("recordOnly = %v, want only recorded-no-hold", gotRecord)
	}
}
