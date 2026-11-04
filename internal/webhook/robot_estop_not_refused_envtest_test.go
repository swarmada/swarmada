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
// A PER-ROBOT ESTOP IS NEVER REFUSED FOR SOMETHING UNRELATED TO THE STOP.
//
// The Robot gate re-checks its invariants on every update, and each invariant resolves
// against another object: the zone's children, the RobotClass, the dock. Any of those
// can change after the robot was admitted without anything refusing the change. The
// per-robot estop is an update, so before this rule it was refused with whatever
// invariant had stopped holding, and the one stop that reaches that robot short of
// namespace scope did not reach it.
//
// Each case below breaks one invariant from outside the robot, then shows three things:
// the trigger is admitted and delivered, the clear is admitted, and an ordinary write to
// the same robot is still refused with the invariant's error. The last is the control:
// the exemption is the estop write, not the invariant.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestEnvtest_PerRobotEstopIsNotRefusedByAnUnrelatedInvariant(t *testing.T) {
	w := startSafetyWire(t)
	estopper := startEstopControllers(t, w)
	markerFixture(t, w, w.ns)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		robot string
		// setUp adapts the robot before it is created.
		setUp func(t *testing.T, r *fleetv1.Robot)
		// breakIt makes one invariant stop holding without touching the robot.
		breakIt func(t *testing.T)
		// refusedWith is what the invariant says on an ordinary write afterwards.
		refusedWith string
	}{
		{
			name:  "its leaf zone gained a child zone",
			robot: "r-leaf",
			setUp: func(t *testing.T, r *fleetv1.Robot) {
				if err := w.admin.Create(ctx, &fleetv1.FleetZone{
					ObjectMeta: metav1.ObjectMeta{Name: "bay-2", Namespace: w.ns},
				}); err != nil {
					t.Fatalf("creating zone bay-2: %v", err)
				}
				r.Spec.Zone = "bay-2"
			},
			breakIt: func(t *testing.T) {
				if err := w.admin.Create(ctx, &fleetv1.FleetZone{
					ObjectMeta: metav1.ObjectMeta{Name: "bay-2-child", Namespace: w.ns},
					Spec:       fleetv1.FleetZoneSpec{ParentZone: "bay-2"},
				}); err != nil {
					t.Fatalf("creating a child under the robot's declared zone: %v", err)
				}
			},
			refusedWith: "is not a leaf zone",
		},
		{
			name:  "its RobotClass was deleted",
			robot: "r-class",
			setUp: func(t *testing.T, r *fleetv1.Robot) {
				if err := w.admin.Create(ctx, &fleetv1.RobotClass{
					ObjectMeta: metav1.ObjectMeta{Name: "picker", Namespace: w.ns},
					Spec: fleetv1.RobotClassSpec{
						Manufacturer: "acme-robotics",
						Model:        "acme-picker",
						BaseAdapter:  fleetv1.BaseAdapterRef{Name: markerRobotAdapter, Version: "1.0.0"},
						Hardware:     []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
					},
				}); err != nil {
					t.Fatalf("creating RobotClass picker: %v", err)
				}
				r.Spec.RobotClass = "picker"
			},
			breakIt: func(t *testing.T) {
				if err := w.admin.Delete(ctx, &fleetv1.RobotClass{
					ObjectMeta: metav1.ObjectMeta{Name: "picker", Namespace: w.ns},
				}); err != nil {
					t.Fatalf("deleting the robot's RobotClass: %v", err)
				}
			},
			refusedWith: `RobotClass "picker" not found`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := markerRobot(w.ns, tc.robot, nil)
			tc.setUp(t, r)
			if err := w.admin.Create(ctx, r); err != nil {
				t.Fatalf("creating the robot while every invariant holds: %v", err)
			}
			tc.breakIt(t)

			// Trigger.
			if err := patchRobot(ctx, w.admin, w.ns, tc.robot, func(cur *fleetv1.Robot) {
				cur.Annotations[estopTriggeredAnnotation] = "true"
			}); err != nil {
				t.Fatalf("the per-robot estop was REFUSED because of an invariant unrelated to the "+
					"stop, so no estop short of namespace scope reaches this robot: %v", err)
			}
			eventually(t, "the per-robot estop reaching "+tc.robot, func() (bool, string) {
				for _, got := range estopper.delivered() {
					if got == tc.robot {
						return true, ""
					}
				}
				return false, "not delivered"
			})

			// Control: an ordinary write is still held to the invariant.
			err := patchRobot(ctx, w.admin, w.ns, tc.robot, func(cur *fleetv1.Robot) {
				if cur.Labels == nil {
					cur.Labels = map[string]string{}
				}
				cur.Labels["team"] = "night"
			})
			if err == nil || !strings.Contains(err.Error(), tc.refusedWith) {
				t.Fatalf("a label write after the invariant broke must still be refused with %q; got %v",
					tc.refusedWith, err)
			}

			// Clear.
			if err := patchRobot(ctx, w.admin, w.ns, tc.robot, func(cur *fleetv1.Robot) {
				delete(cur.Annotations, estopTriggeredAnnotation)
			}); err != nil {
				t.Fatalf("the per-robot estop CLEAR was refused because of an unrelated invariant: %v", err)
			}
		})
	}
}

// patchRobot reads the robot, applies mutate to a copy with a non-nil annotation map,
// and merge-patches the difference, as swarmctl and kubectl annotate do.
func patchRobot(ctx context.Context, c client.Client, ns, name string, mutate func(*fleetv1.Robot)) error {
	cur := &fleetv1.Robot{}
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

// TestOnlyEstopAnnotationsChanged pins what the exemption covers, so it cannot widen
// into "any annotation write" unnoticed.
func TestOnlyEstopAnnotationsChanged(t *testing.T) {
	base := func() *fleetv1.Robot {
		return &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{
				Name: "r", Namespace: "ns", ResourceVersion: "7",
				Annotations: map[string]string{fleetv1.RobotIDAnnotation: "r", annLengthUnit: "Meters"},
				Labels:      map[string]string{"a": "b"},
			},
			Spec: fleetv1.RobotSpec{Zone: "bay-1"},
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*fleetv1.Robot)
		want   bool
	}{
		{"trigger added", func(r *fleetv1.Robot) { r.Annotations[estopTriggeredAnnotation] = "true" }, true},
		{"trigger with the stamped actor", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.Annotations[AnnEstopActor] = "alice"
		}, true},
		{"trigger with a restamped coordinate convention", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.Annotations[annLengthUnit] = "Feet"
		}, true},
		{"server-managed metadata differs", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.ResourceVersion = "8"
			r.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}
		}, true},
		{"nothing changed", func(*fleetv1.Robot) {}, false},
		{"trigger plus a label", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.Labels["team"] = "night"
		}, false},
		{"trigger plus a robot-id change", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.Annotations[fleetv1.RobotIDAnnotation] = "someone-else"
		}, false},
		{"trigger plus a spec change", func(r *fleetv1.Robot) {
			r.Annotations[estopTriggeredAnnotation] = "true"
			r.Spec.Zone = "bay-2"
		}, false},
		{"a defaulter stamp alone", func(r *fleetv1.Robot) { r.Annotations[annLengthUnit] = "Feet" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldR, newR := base(), base()
			tc.mutate(newR)
			if got := onlyEstopAnnotationsChanged(oldR, newR); got != tc.want {
				t.Fatalf("onlyEstopAnnotationsChanged = %v, want %v", got, tc.want)
			}
		})
	}
	// A clear: the trigger removed.
	oldR, newR := base(), base()
	oldR.Annotations[estopTriggeredAnnotation] = "true"
	if !onlyEstopAnnotationsChanged(oldR, newR) {
		t.Fatal("a clear (trigger removed) must be exempt")
	}
}
