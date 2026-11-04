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
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Rows over forRobot and the refusal sentence directly. The containment fixtures are in
// dispatch_operating_constraints_containing_test.go.

// FA-R2's set is FOLDED, not concatenated (FZ-R11): two bounds on one subject take the
// tighter, and the rule names the zone that declared it.
func TestFilter12_TheContainingSetIsFoldedPerFZR11(t *testing.T) {
	bay := operatingRule("speed", fleetv1.ZoneRestrictionLimited, "ward-bay", "ward pace")
	bay.Max = "1.0"
	corridor := operatingRule("speed", fleetv1.ZoneRestrictionLimited, "corridor", "corridor pace")
	corridor.Max = "0.5"
	zones := []fleetv1.FleetZone{zrZone("ward", ""), zrZone("ward-bay", "ward", bay), zrZone("corridor", "", corridor)}
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1", "speed"), "ward-bay", "corridor", "ward-bay")

	got, unresolved := containingState(action, zones).operating.forRobot(robot)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none", unresolved)
	}
	if len(got) != 1 || got[0].Subject != "speed" || got[0].Max != "0.5" || got[0].InheritedFrom != "corridor" {
		t.Fatalf("in force = %+v, want ONE speed rule, max 0.5, declared by corridor: a union that "+
			"kept both entries would hand the adapter two bounds to choose between", got)
	}
}

// The refusal sentence names the rule of a containing zone outside the subtree, and a
// containing zone whose rules could not be read.
func TestFilter12_TheRefusalNamesTheContainingZonesRules(t *testing.T) {
	zones := corridorZones()
	zones = append(zones, zrZone("annex", ""))
	zones[3].Status.Conditions = nil // never reconciled: unresolved
	action := zrAction("ward")
	held := *standingIn(f12Robot("amr-1"), "ward-bay", "annex", "corridor", "ward-bay")
	other := *standingIn(f12Robot("amr-2"), "ward-bay", "ward-bay")
	st := containingState(action, zones)
	ex := st.excludedBy(&held, action)
	if ex == nil {
		t.Fatalf("amr-1 was not withheld")
	}

	msg := operatingConstraintMessage(action, st.operating,
		[]dispatchExclusion{{Robot: "amr-1", Filter: ex.Filter, Reason: ex.Reason}},
		[]fleetv1.Robot{held, other})
	for _, want := range []string{"microphone", `"corridor"`, "in force in: corridor", "annex", "amr-1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
	if again := operatingConstraintMessage(action, st.operating,
		[]dispatchExclusion{{Robot: "amr-1", Filter: ex.Filter, Reason: ex.Reason}},
		[]fleetv1.Robot{other, held}); again != msg {
		t.Errorf("the sentence depends on list order (RA-1: a Pending action re-patches it every "+
			"requeue):\n%q\n%q", msg, again)
	}
}
