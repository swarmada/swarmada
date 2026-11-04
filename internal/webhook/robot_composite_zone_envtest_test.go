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
// A COMPOSITE ZONE IS NOT A PLACE A ROBOT CAN BE (RFC-0001 FZ-C2).
//
// Two rules, each correct alone, combined into a robot no zone estop can reach:
//
//   - the Robot admission gate tested terminality by child zones only, and a composite
//     has members, not children, so a Robot could declare one;
//   - a composite has no polygon, so FZ-G5 placed that robot in it by declaration and
//     status.currentZone named the composite;
//   - FZ-C5 refuses a zone estop on a composite, which was then the only zone the robot
//     reported.
//
// The first test is the refusal. The second is the migration case: a robot that already
// declares a zone which has since become a composite must still be stoppable per robot.
// The FleetZone webhook now refuses members added to a zone a robot declares, so that
// state is held only by a cluster that admitted it before the rule, or through a Robot
// and a membership change admitted concurrently; the test builds it with that webhook
// unregistered for the one write.
// The gate re-checks its invariants on every update, so a check that ran on an
// annotation-only write would refuse the per-robot estop as well, and the fix would
// strand exactly the robots it exists to protect.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// createComposite creates a composite zone grouping members.
func createComposite(t *testing.T, w *safetyWire, name string, members ...string) {
	t.Helper()
	if err := w.admin.Create(context.Background(), &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
		Spec:       fleetv1.FleetZoneSpec{MemberZones: members},
	}); err != nil {
		t.Fatalf("creating composite zone %s: %v", name, err)
	}
}

// refusedAsComposite fails unless err is the gate refusing spec.zone for naming a composite.
func refusedAsComposite(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: a Robot whose spec.zone names a composite zone was ADMITTED", what)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("%s: refused, but not as an invalid spec.zone: %v", what, err)
	}
	for _, want := range []string{"spec.zone", "composite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: the refusal does not say %q: %v", what, want, err)
		}
	}
}

func TestEnvtest_RobotSpecZoneRefusesACompositeZone(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	createComposite(t, w, "ward", markerRobotZone)

	t.Run("CreateIntoAComposite", func(t *testing.T) {
		r := markerRobot(w.ns, "into-composite", nil)
		r.Spec.Zone = "ward"
		refusedAsComposite(t, "create", w.admin.Create(ctx, r))
	})

	t.Run("MoveIntoAComposite", func(t *testing.T) {
		// The control: the same robot in a terminal zone is admitted.
		r := markerRobot(w.ns, "moves-into-composite", nil)
		if err := w.admin.Create(ctx, r); err != nil {
			t.Fatalf("creating the robot in terminal zone %s: %v", markerRobotZone, err)
		}
		base := r.DeepCopy()
		r.Spec.Zone = "ward"
		refusedAsComposite(t, "update", w.admin.Patch(ctx, r, client.MergeFrom(base)))
	})
}

func TestEnvtest_RobotAlreadyInACompositeIsStillStoppablePerRobot(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	estopper := startEstopControllers(t, w)
	markerFixture(t, w, w.ns)

	// The robot is admitted while its zone is terminal. The zone then gains members, so
	// the robot now declares a composite. This is also the shape of any robot admitted
	// before the gate tested members.
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "ward-2", Namespace: w.ns},
	}); err != nil {
		t.Fatalf("creating zone ward-2: %v", err)
	}
	r := markerRobot(w.ns, "stranded", nil)
	r.Spec.Zone = "ward-2"
	if err := w.admin.Create(ctx, r); err != nil {
		t.Fatalf("creating the robot in ward-2 while it is terminal: %v", err)
	}
	writeWithoutTheFleetZoneWebhook(t, w, func() error {
		return patchFleetZone(ctx, w.admin, w.ns, "ward-2", func(z *fleetv1.FleetZone) {
			z.Spec.MemberZones = []string{markerRobotZone}
		})
	})
	ward := &fleetv1.FleetZone{}
	if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "ward-2"}, ward); err != nil {
		t.Fatal(err)
	}

	// The premise: a zone estop on the composite is refused (FZ-C5), so the per-robot
	// estop is the one this robot has left.
	zbase := ward.DeepCopy()
	ward.Annotations = map[string]string{estopTriggeredAnnotation: "true"}
	if err := w.admin.Patch(ctx, ward, client.MergeFrom(zbase)); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a zone estop on composite ward-2 must be Forbidden (FZ-C5), got: %v", err)
	}

	cur := &fleetv1.Robot{}
	if err := w.admin.Get(ctx, client.ObjectKeyFromObject(r), cur); err != nil {
		t.Fatal(err)
	}
	rbase := cur.DeepCopy()
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations[estopTriggeredAnnotation] = "true"
	if err := w.admin.Patch(ctx, cur, client.MergeFrom(rbase)); err != nil {
		t.Fatalf("the per-robot estop on a robot that declares a composite was REFUSED, so the "+
			"robot is reachable by no estop short of namespace scope: %v", err)
	}
	eventually(t, "the per-robot estop reaching the robot", func() (bool, string) {
		for _, got := range estopper.delivered() {
			if got == "stranded" {
				return true, ""
			}
		}
		return false, "not delivered"
	})
}
