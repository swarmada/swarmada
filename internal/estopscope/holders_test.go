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

package estopscope

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func recorded(r *fleetv1.Robot, kind fleetv1.EstopHoldKind, name, uid string) *fleetv1.Robot {
	r.Status.EstopHeldBy = append(r.Status.EstopHeldBy, fleetv1.EstopHold{Kind: kind, Name: name, UID: uid})
	return r
}

func withUID(z fleetv1.FleetZone, uid string) fleetv1.FleetZone {
	z.UID = types.UID(uid)
	return z
}

func marked(z fleetv1.FleetZone) fleetv1.FleetZone {
	if z.Annotations == nil {
		z.Annotations = map[string]string{}
	}
	z.Annotations[ProcessedAnnotation] = "x"
	return z
}

// A stop recorded on the robot holds it wherever the zone tree, the robot's zone and its
// position stand now, until its carrier's clear.
func TestHolders_TheRecordHoldsWhateverTheTree(t *testing.T) {
	cases := []struct {
		name    string
		robot   *fleetv1.Robot
		zones   []fleetv1.FleetZone
		configs []fleetv1.SwarmadaConfig
		want    []Holder
	}{
		{name: "moved out of the stopped zone",
			robot: recorded(robot("elsewhere", "", nil), fleetv1.EstopHoldFleetZone, "z", "u1"),
			zones: []fleetv1.FleetZone{withUID(zone("z", "", p("z-stop"), nil), "u1"), zone("elsewhere", "", nil, nil)},
			want:  []Holder{{Kind: KindZone, Name: "z", Value: "z-stop", Recorded: true}}},
		{name: "its zone re-parented away from the stopped ancestor",
			robot: recorded(robot("c", "", nil), fleetv1.EstopHoldFleetZone, "anc", "u1"),
			zones: []fleetv1.FleetZone{withUID(zone("anc", "", p("a"), nil), "u1"), zone("c", "", nil, nil)},
			want:  []Holder{{Kind: KindZone, Name: "anc", Value: "a", Recorded: true}}},
		{name: "the ancestor stopped propagating",
			robot: recorded(robot("c", "", nil), fleetv1.EstopHoldFleetZone, "anc", "u1"),
			zones: []fleetv1.FleetZone{withUID(zone("anc", "", p("a"), p(false)), "u1"), zone("c", "anc", nil, nil)},
			want:  []Holder{{Kind: KindZone, Name: "anc", Value: "a", Recorded: true}}},
		{name: "a clear in flight still holds (marker, no trigger)",
			robot: recorded(robot("z", "", nil), fleetv1.EstopHoldFleetZone, "z", "u1"),
			zones: []fleetv1.FleetZone{withUID(marked(zone("z", "", nil, nil)), "u1")},
			want:  []Holder{{Kind: KindZone, Name: "z", Recorded: true, Clearing: true}}},
		{name: "a cleared carrier's leftover record does not hold",
			robot: recorded(robot("z", "", nil), fleetv1.EstopHoldFleetZone, "z", "u1"),
			zones: []fleetv1.FleetZone{withUID(zone("z", "", nil, nil), "u1")}},
		{name: "a carrier that no longer exists holds, last, as orphaned",
			robot: recorded(robot("y", "", nil), fleetv1.EstopHoldFleetZone, "gone", "u1"),
			zones: []fleetv1.FleetZone{zone("y", "", nil, nil)}, configs: []fleetv1.SwarmadaConfig{config(p("ns"))},
			want: []Holder{{Kind: KindNamespace, Name: "swarmada-config", Value: "ns"},
				{Kind: KindZone, Name: "gone", Recorded: true, Orphaned: true}}},
		{name: "a carrier recreated under another UID is orphaned",
			robot: recorded(robot("z", "", nil), fleetv1.EstopHoldFleetZone, "z", "old"),
			zones: []fleetv1.FleetZone{withUID(zone("z", "", nil, nil), "new")},
			want:  []Holder{{Kind: KindZone, Name: "z", Recorded: true, Orphaned: true}}},
		{name: "the namespace stop recorded, its config's trigger removed and the clear in flight",
			robot: recorded(robot("", "", nil), fleetv1.EstopHoldSwarmadaConfig, "swarmada-config", ""),
			configs: []fleetv1.SwarmadaConfig{func() fleetv1.SwarmadaConfig {
				c := config(nil)
				c.Annotations = map[string]string{ProcessedAnnotation: "n"}
				return c
			}()},
			want: []Holder{{Kind: KindNamespace, Name: "swarmada-config", Recorded: true, Clearing: true}}},
		{name: "the own trigger first, the record next, the tree after, each once",
			robot: recorded(robot("z", "", p("own")), fleetv1.EstopHoldFleetZone, "z", ""),
			zones: []fleetv1.FleetZone{zone("z", "", p("z-stop"), nil)},
			want: []Holder{{Kind: KindRobot, Name: "r1", Value: "own"},
				{Kind: KindZone, Name: "z", Value: "z-stop", Recorded: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Holders(tc.robot, tc.zones, tc.configs)
			if len(got) != len(tc.want) {
				t.Fatalf("Holders = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Holders[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHeldOnRecord(t *testing.T) {
	if _, held := HeldOnRecord(robot("z", "", nil)); held {
		t.Fatal("a robot with no trigger and no record is held on record")
	}
	if h, held := HeldOnRecord(robot("z", "", p(""))); !held || h.Kind != KindRobot {
		t.Fatalf("an empty own trigger: %+v %v", h, held)
	}
	r := recorded(robot("", "", nil), fleetv1.EstopHoldSwarmadaConfig, "swarmada-config", "")
	if h, held := HeldOnRecord(r); !held || h.Kind != KindNamespace || !h.Recorded {
		t.Fatalf("a recorded namespace stop: %+v %v", h, held)
	}
}

func TestHolderString_Orphaned(t *testing.T) {
	h := Holder{Kind: KindZone, Name: "z", Recorded: true, Orphaned: true}
	if got, want := h.String(), "the estop of zone z, which no longer exists (recorded in status.estopHeldBy)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
