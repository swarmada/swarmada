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
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func chainTree(depth int, stopAt string, pending bool) *Tree {
	zs := zoneChain(depth)
	for i := range zs {
		zs[i].Annotations = nil
		if zs[i].Name == stopAt {
			zs[i].Annotations = map[string]string{TriggerAnnotation: "s"}
			if !pending {
				zs[i].Annotations[ProcessedAnnotation] = "s"
			}
		}
	}
	return TreeOf(zs)
}

func TestTree_ReachingStopsAtTheBoundAndFailsClosedPastIt(t *testing.T) {
	// z64 has 64 ancestors: the deepest zone admission admits. z0's stop reaches it.
	tr := chainTree(64, "z0", true)
	got, err := tr.Reaching("z64", Carries)
	if err != nil || len(got) != 1 || got[0] != "z0" {
		t.Fatalf("z0's stop must reach z64 (64 ancestors): %v %v", got, err)
	}
	// z65 has 65 ancestors: the walk cannot finish, and says so.
	tr = chainTree(65, "z0", true)
	var deep *TooDeepError
	if _, err := tr.Reaching("z65", Carries); !errors.As(err, &deep) {
		t.Fatalf("a chain of 65 ancestors must be an error, got %v", err)
	}
	if d, _ := tr.Depth("z64"); d != 64 {
		t.Errorf("z64 depth %d, want 64", d)
	}
	if _, _, err := tr.DeepestBelow("z0"); !errors.As(err, &deep) {
		t.Errorf("the deepest zone below z0 is past the bound: want an error, got %v", err)
	}
}

func TestTree_WithAndWithoutModelAnEdit(t *testing.T) {
	tr := chainTree(3, "z0", false)
	moved := tr.Zone("z1").DeepCopy()
	moved.Spec.ParentZone = ""
	after := tr.With(moved)
	if got, _ := after.Reaching("z3", Carries); len(got) != 0 {
		t.Errorf("after re-parenting z1 to the root, z0's stop must not reach z3: %v", got)
	}
	if got, _ := tr.Reaching("z3", Carries); len(got) != 1 {
		t.Errorf("the original tree is unchanged: %v", got)
	}
	if after.Without("z2").Zone("z2") != nil {
		t.Errorf("Without keeps the zone")
	}
	sub, err := tr.Subtree("z1")
	if err != nil || len(sub) != 3 {
		t.Errorf("subtree of z1 = %v %v, want z1 z2 z3", sub, err)
	}
}

func TestTree_CoversIsTheFanOutsQuestion(t *testing.T) {
	tr := chainTree(64, "z0", false)
	if ok, err := tr.Covers("z0", "z64", true); !ok || err != nil {
		t.Errorf("z0 covers z64 with propagation: %v %v", ok, err)
	}
	if ok, _ := tr.Covers("z0", "z64", false); ok {
		t.Errorf("z0 does not cover z64 without propagation")
	}
	if ok, _ := tr.Covers("z0", "z0", false); !ok {
		t.Errorf("a zone covers itself whatever its policy")
	}
	if ok, _ := tr.Covers("z0", "", true); ok {
		t.Errorf("no zone, no coverage")
	}
}

func TestCoverageOf_ReadsBothZonesTheRecordAndTheNamespace(t *testing.T) {
	tr := chainTree(2, "z0", true)
	cfg := fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: "ns",
		Annotations: map[string]string{ProcessedAnnotation: "n"}}}
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
	robot.Spec.Zone = "z2"
	robot.Status.CurrentZone = "elsewhere"
	robot.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "gone"}}
	robot.DeletionTimestamp = &metav1.Time{}
	cov, err := CoverageOf(tr, []fleetv1.SwarmadaConfig{cfg}, robot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"zone/z0", "namespace/swarmada-config", "zone/gone"}
	if fmt.Sprint(cov.Names()) != fmt.Sprint(want) {
		t.Errorf("coverage %v, want %v (declared zone's pending stop, the namespace, the orphaned record)", cov.Names(), want)
	}
	if !cov.Pending() || !cov.Has(KindZone, "z0") {
		t.Errorf("z0's stop is pending and covers the robot: %+v", cov)
	}
	if lost := Lost(cov, Coverage{}); len(lost) != 3 {
		t.Errorf("everything is lost against nothing: %v", lost)
	}
}

func TestTree_ACycleIsAnError(t *testing.T) {
	a := fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}, Spec: fleetv1.FleetZoneSpec{ParentZone: "b"}}
	b := fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}, Spec: fleetv1.FleetZoneSpec{ParentZone: "a"}}
	tr := TreeOf([]fleetv1.FleetZone{a, b})
	if _, err := tr.Ancestors("a"); err == nil {
		t.Errorf("a cycle must not be walked to an answer")
	}
	if _, err := tr.Subtree("a"); err == nil {
		t.Errorf("a cycle below the root must not be walked to an answer")
	}
}
