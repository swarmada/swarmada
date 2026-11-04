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
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// zoneChain is zones z0 (the root, carrying a pending stop) to z<depth>: z<depth> has depth ancestors.
func zoneChain(depth int) []fleetv1.FleetZone {
	zs := []fleetv1.FleetZone{{ObjectMeta: metav1.ObjectMeta{Name: "z0", Namespace: "ns",
		Annotations: map[string]string{TriggerAnnotation: "stop"}}}}
	for i := 1; i <= depth; i++ {
		zs = append(zs, fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("z%d", i), Namespace: "ns"},
			Spec: fleetv1.FleetZoneSpec{ParentZone: fmt.Sprintf("z%d", i-1)}})
	}
	return zs
}

// chain is a reader holding zoneChain(depth).
func chain(t *testing.T, depth int) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	var objs []client.Object
	for _, z := range zoneChain(depth) {
		objs = append(objs, z.DeepCopy())
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// The pending walk reaches as far as the fan-out does, a zone and walkCap ancestors above it (the
// fan-out stops a robot whose zone has walkCap ancestors, and admission admits such a zone), and
// a chain still climbing past that is an error, which every caller treats as refused or held.
func TestPendingStopsOver_ReachesTheFanOutBoundAndFailsClosedPastIt(t *testing.T) {
	ctx := context.Background()
	for _, depth := range []int{34, walkCap - 1, walkCap} {
		got, err := PendingStopsOver(ctx, chain(t, depth), "ns", fmt.Sprintf("z%d", depth), true)
		if err != nil || len(got) != 1 || got[0] != "z0" {
			t.Errorf("depth %d: the pending stop of z0 is not found: %v, %v", depth, got, err)
		}
	}
	got, err := PendingStopsOver(ctx, chain(t, walkCap+1), "ns", fmt.Sprintf("z%d", walkCap+1), true)
	if err == nil {
		t.Errorf("a chain deeper than %d zones was walked to an answer (%v) instead of failing closed", walkCap, got)
	}
}

// Holders, which answers every resume and every clear, finds the stop of a zone walkCap
// ancestors above the robot's, as the fan-out does.
func TestHolders_FindsTheStopOfTheFarthestAncestorTheFanOutReaches(t *testing.T) {
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns"},
		Spec: fleetv1.RobotSpec{Zone: fmt.Sprintf("z%d", walkCap)}}
	found := false
	for _, h := range Holders(robot, zoneChain(walkCap), nil) {
		if h.Kind == KindZone && h.Name == "z0" {
			found = true
		}
	}
	if !found {
		t.Errorf("the stop of z0, %d ancestors above the robot's zone, is not among its holders", walkCap)
	}
}
