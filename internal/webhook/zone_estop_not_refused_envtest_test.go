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

// ─────────────────────────────────────────────────────────────────────────────
// A ZONE ESTOP IS NEVER REFUSED FOR SOMETHING UNRELATED TO THE STOP, OVER THE WIRE.
//
// site is tightened after bay was admitted beneath it; FZ-R8 admits that, and bay then
// contradicts its ancestor. The zone estop on bay is admitted, the shipped zone-estop
// controller fans it out and records the processed marker (a write of its own that was
// refused the same way), the clear is admitted, and an ordinary write to bay is still
// refused with FZ-R7's error. The last is the control: the exemption is the estop write,
// not the invariant.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestEnvtest_ZoneEstopIsNotRefusedAfterAnAncestorTightens(t *testing.T) {
	w := startSafetyWire(t)
	estopper := startEstopControllers(t, w)
	markerFixture(t, w, w.ns)
	ctx := context.Background()

	camera := func(mode fleetv1.ZoneRestrictionMode, max string) *fleetv1.ZoneRestrictions {
		return &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{{Subject: "camera", Mode: mode, Max: max}}}
	}
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: w.ns},
	}); err != nil {
		t.Fatalf("creating site: %v", err)
	}
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: w.ns},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: "site", Restrictions: camera(fleetv1.ZoneRestrictionLimited, "3")},
	}); err != nil {
		t.Fatalf("creating bay under an unrestricted site: %v", err)
	}
	r := markerRobot(w.ns, "r-bay", nil)
	r.Spec.Zone = "bay"
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating a robot in bay: %v", err)
	}
	if err := patchFleetZone(ctx, w.admin, w.ns, "site", func(z *fleetv1.FleetZone) {
		z.Spec.Restrictions = camera(fleetv1.ZoneRestrictionDenied, "")
	}); err != nil {
		t.Fatalf("tightening site onto bay must be admitted (FZ-R8): %v", err)
	}

	// Control: bay now contradicts site, and an ordinary write says so.
	err := patchFleetZone(ctx, w.admin, w.ns, "bay", func(z *fleetv1.FleetZone) {
		z.Labels = map[string]string{"team": "night"}
	})
	refusedSaying(t, "a label write on bay after site tightened", err, fzr7Says)

	// Trigger.
	if err := patchFleetZone(ctx, w.admin, w.ns, "bay", func(z *fleetv1.FleetZone) {
		z.Annotations[estopTriggeredAnnotation] = "true"
	}); err != nil {
		t.Fatalf("the zone estop on bay was REFUSED because its ancestor tightened after it was admitted: %v", err)
	}
	eventually(t, "the zone estop on bay reaching r-bay", func() (bool, string) {
		for _, got := range estopper.delivered() {
			if got == "r-bay" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("delivered so far: %v", estopper.delivered())
	})
	eventually(t, "the zone-estop controller recording bay's trigger as processed", func() (bool, string) {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "bay"}, cur); err != nil {
			return false, err.Error()
		}
		return cur.Annotations[estopProcessedAnnotation] == "true",
			fmt.Sprintf("processed marker = %q", cur.Annotations[estopProcessedAnnotation])
	})

	// Clear.
	if err := patchFleetZone(ctx, w.admin, w.ns, "bay", func(z *fleetv1.FleetZone) {
		delete(z.Annotations, estopTriggeredAnnotation)
	}); err != nil {
		t.Fatalf("the zone estop CLEAR on bay was refused because of an unrelated invariant: %v", err)
	}
	eventually(t, "the zone-estop controller dropping bay's processed marker after the clear", func() (bool, string) {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "bay"}, cur); err != nil {
			return false, err.Error()
		}
		_, has := cur.Annotations[estopProcessedAnnotation]
		return !has, fmt.Sprintf("processed marker = %q", cur.Annotations[estopProcessedAnnotation])
	})
}

// TestEnvtest_MembersAreRefusedOnAZoneARobotDeclares is the route by which a robot came
// to declare a composite: members added to its zone after it was admitted.
func TestEnvtest_MembersAreRefusedOnAZoneARobotDeclares(t *testing.T) {
	w := startSafetyWire(t)
	markerFixture(t, w, w.ns)
	ctx := context.Background()

	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "ward-4", Namespace: w.ns},
	}); err != nil {
		t.Fatalf("creating zone ward-4: %v", err)
	}
	r := markerRobot(w.ns, "r-ward", nil)
	r.Spec.Zone = "ward-4"
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating the robot in ward-4 while it is terminal: %v", err)
	}
	err := patchFleetZone(ctx, w.admin, w.ns, "ward-4", func(z *fleetv1.FleetZone) {
		z.Spec.MemberZones = []string{markerRobotZone}
	})
	refusedSaying(t, "members added to ward-4, which r-ward declares", err, "spec.memberZones", "r-ward")

	// Once the robot is re-homed, the zone may become a composite.
	if err := patchRobot(ctx, w.admin, w.ns, "r-ward", func(cur *fleetv1.Robot) {
		cur.Spec.Zone = markerRobotZone
	}); err != nil {
		t.Fatalf("re-homing r-ward to %s: %v", markerRobotZone, err)
	}
	if err := patchFleetZone(ctx, w.admin, w.ns, "ward-4", func(z *fleetv1.FleetZone) {
		z.Spec.MemberZones = []string{markerRobotZone}
	}); err != nil {
		t.Fatalf("ward-4, now declared by no robot, must be able to become a composite: %v", err)
	}
}

// patchFleetZone reads the zone, applies mutate to a copy with a non-nil annotation map,
// and merge-patches the difference, as swarmctl and kubectl annotate do.
func patchFleetZone(ctx context.Context, c client.Client, ns, name string, mutate func(*fleetv1.FleetZone)) error {
	cur := &fleetv1.FleetZone{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cur); err != nil {
		return err
	}
	base := cur.DeepCopy()
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	mutate(cur)
	return c.Patch(ctx, cur, client.MergeFrom(base))
}
