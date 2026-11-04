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
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// Candidate filter 9's version condition (RFC-0001 #control-plane-scheduler, filter 9): "The
// adapter's connection MUST also have negotiated contract 1.4.0 (status.negotiatedContractVersion):
// a robot whose adapter is not bound by the Stop Latch is not given work." It is held behind
// RequireLatchContract, off in the manager at v0.4, because every in-tree deployment runs the
// simulation adapter at 1.3.0. These rows run it on.

// adapterNegotiated is the ready fixture adapter whose connection negotiated version.
func adapterNegotiated(version string) *fleetv1.FleetAdapter {
	a := readyActionAdapter()
	a.Status.NegotiatedContractVersion = version
	return a
}

func TestAssignmentGate_LatchContract_WithholdsARobotWhoseAdapterNegotiatedBelow140(t *testing.T) {
	for _, tc := range []struct {
		name, negotiated string
		want             fleetv1.ActionPhase
	}{
		{"1.3.0", "1.3.0", fleetv1.ActionPhasePending},
		{"1.3.2", "1.3.2", fleetv1.ActionPhasePending},
		// Fail closed: an adapter that records no agreed version is not known to be bound.
		{"none recorded", "", fleetv1.ActionPhasePending},
		{"unreadable", "one.four", fleetv1.ActionPhasePending},
		// The control: the gate withholds only what it must.
		{"1.4.0", contract.Version, fleetv1.ActionPhaseAssigned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newActionReconciler(t, adapterNegotiated(tc.negotiated),
				pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			r.RequireLatchContract = true
			reconcileAction(t, r, "t1")
			if got := phaseOfAction(t, c, "t1"); got != tc.want {
				t.Fatalf("phase = %s, want %s for an adapter that negotiated %q", got, tc.want, tc.negotiated)
			}
			if tc.want == fleetv1.ActionPhaseAssigned {
				return
			}
			var robot fleetv1.Robot
			if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: actionNS}, &robot); err != nil {
				t.Fatalf("get robot: %v", err)
			}
			if robot.Status.AssignedAction != "" {
				t.Errorf("robot bound to %q though its adapter negotiated %q", robot.Status.AssignedAction, tc.negotiated)
			}
		})
	}
}

// Off, the condition reads nothing: a 1.3.0 connection is dispatched to, as at v0.4. This is the
// state the manager runs in, and the row the filter-9 Not-met note describes.
func TestAssignmentGate_LatchContract_OffDispatchesToA130Connection(t *testing.T) {
	r, c := newActionReconciler(t, adapterNegotiated("1.3.0"),
		pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned with RequireLatchContract off", got)
	}
}
