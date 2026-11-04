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
	"errors"
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Row A15's send check, its version bullet (RFC-0001 #safety-action-table, A15): an AssignAction
// is sent only on a ControlStream that negotiated contract 1.4.0. A 1.3.x connection is sent every
// stop and its fence, and no AssignAction (#fleet-adapter-protocol-package-and-versioning). Held
// behind RequireContract, off in the manager at v0.4; these rows run it on.

func TestAssign_LatchContract_RefusesAStreamBelow140AndSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, negotiated string
		told             bool
		sent             bool
	}{
		{"1.3.0", "1.3.0", true, false},
		{"none agreed", "", true, false},
		// Fail closed: a stream whose version this dispatcher was never told is not known to be bound.
		{"never told", "", false, false},
		{"1.4.0", "1.4.0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newDispatcher(t, cmdAdapter)
			d.RequireContract = true
			s := acceptAll(d)
			d.RegisterStream(verifiedIdentity(), s)
			if tc.told {
				d.StreamNegotiated(s, tc.negotiated)
			}
			err := assignOnce(d)
			assigns := countAssigns(s.sent)
			if tc.sent {
				if err != nil || assigns != 1 {
					t.Fatalf("err %v, %d AssignActions on the wire; want it sent on a %s stream", err, assigns, tc.negotiated)
				}
				return
			}
			if !errors.Is(err, ErrBelowLatchContract) || assigns != 0 {
				t.Fatalf("err %v, %d AssignActions on the wire; want ErrBelowLatchContract and nothing sent", err, assigns)
			}
		})
	}
}

// The check withholds work only: a 1.3.0 stream is still sent a stop's fence.
func TestAssign_LatchContract_AStreamBelow140IsStillSentTheFence(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	d.RequireContract = true
	s := acceptAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	d.StreamNegotiated(s, "1.3.0")
	if err := d.SendFence(cmdRobot(t, c), "s1", &fav1.Estop{Reason: "test"}); err != nil {
		t.Fatalf("fence: %v", err)
	}
	fences := 0
	for _, cmd := range s.sent {
		if cmd.GetEstop().GetEstopId() == "s1" {
			fences++
		}
	}
	if fences != 1 {
		t.Fatalf("%d fences on the 1.3.0 stream, want 1: every stop goes to every connection", fences)
	}
}

// Off, the version is not read: work goes to a 1.3.0 stream, as at v0.4.
func TestAssign_LatchContract_OffSendsToA130Stream(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	s := acceptAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	d.StreamNegotiated(s, "1.3.0")
	if err := assignOnce(d); err != nil || countAssigns(s.sent) != 1 {
		t.Fatalf("err %v; want the AssignAction sent with RequireContract off", err)
	}
}

func countAssigns(sent []*fav1.Command) int {
	n := 0
	for _, cmd := range sent {
		if cmd.GetAssignAction() != nil {
			n++
		}
	}
	return n
}
