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
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The robot already reads Stopped (an earlier episode never cleared). Two triggers start
// and both read Stopped. The older one reaches the end of its delivery window and writes
// Failed; the newer one is then confirmed STOPPED. Its own copy of the Robot still says
// Stopped, so a comparison against that copy writes nothing and the robot reads Failed
// although its last acknowledgement was STOPPED. The write is decided against the stored
// state, so the confirmed stop is recorded (RFC-0001 #safety-estop-state-machine).
func TestEstopOutcome_ANewerConfirmedStopIsWrittenWhenItsRobotCopyIsStale(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopStopped)
	d.deliveryTimeout = 400 * time.Millisecond
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)
	heldStopResent(t, s)

	older := triggerAsync(d, "a")
	nextEstop(t, s, "the older trigger")
	time.Sleep(200 * time.Millisecond)
	newer := triggerAsync(d, "b")
	b := nextEstop(t, s, "the newer trigger")

	if o := outcome(t, older); o.res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("the older trigger returned %+v; want Failed at the end of its delivery window", o.res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("after the older trigger timed out the robot reads %s; want Failed", got)
	}
	if err := d.RouteAck(identity("acme"), s, "amr-1",
		ackState(b.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, newer); !o.res.Confirmed || o.res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("the newer trigger returned %+v; want confirmed Stopped", o.res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("the newest trigger was confirmed STOPPED and the robot reads %s; want Stopped", got)
	}
}

// The same stale copy in the other direction. The robot reads Failed and two triggers
// start. The older one is confirmed STOPPED and writes Stopped; the newer one then reaches
// the end of its delivery window. Its copy still says Failed, so a comparison against that
// copy writes nothing and the robot reads Stopped, though the newest outcome is Failed. A
// newer trigger's Failed overwrites an older confirmed Stopped: the newest outcome stands.
func TestEstopOutcome_ANewerFailureIsWrittenWhenItsRobotCopyIsStale(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopFailed)
	d.deliveryTimeout = 400 * time.Millisecond
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)
	heldStopResent(t, s)

	older := triggerAsync(d, "a")
	a := nextEstop(t, s, "the older trigger")
	newer := triggerAsync(d, "b")
	nextEstop(t, s, "the newer trigger")

	if err := d.RouteAck(identity("acme"), s, "amr-1",
		ackState(a.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, older); !o.res.Confirmed {
		t.Fatalf("the older trigger returned %+v; want confirmed", o.res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("after the older trigger was confirmed the robot reads %s; want Stopped", got)
	}
	if o := outcome(t, newer); o.res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("the newer trigger returned %+v; want Failed at the end of its delivery window", o.res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
		t.Fatalf("the newest trigger resolved Failed and the robot reads %s; want Failed", got)
	}
}
