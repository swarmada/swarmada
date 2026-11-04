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
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A cleared stop's fence answers a report of that stop (RFC-0001 #safety-robot-stop-table, Reported
// stop IDs and local-latch reports): it goes on the robot's ControlStream, in order with the
// AssignActions, so the adapter's EstopClear for the ID can take effect, and it puts the robot under
// no stop. Work still goes to the robot after it.
func TestClearedFence_IsSentAndHoldsNoWork(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	s := acceptAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	if err := d.SendClearedFence(cmdRobot(t, c), "s-old", &fav1.Estop{Reason: "cleared"}); err != nil {
		t.Fatalf("cleared fence: %v", err)
	}
	fences := 0
	for _, cmd := range s.sent {
		if cmd.GetEstop().GetEstopId() == "s-old" {
			fences++
		}
	}
	if fences != 1 {
		t.Fatalf("%d fences named s-old on the stream, want 1", fences)
	}
	if held := d.HeldStops(cmdNS, cmdRobotID); len(held) != 0 {
		t.Fatalf("stops in force %v after a cleared stop's fence, want none", held)
	}
	if err := assignOnce(d); err != nil {
		t.Fatalf("assign after a cleared stop's fence: %v; want it sent", err)
	}
}
