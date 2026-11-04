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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// recordingForgetter records which namespace/robot_id pairs the resetter asked the
// projector to forget.
type recordingForgetter struct{ forgot []string }

func (f *recordingForgetter) ForgetRobot(namespace, robotID string) bool {
	f.forgot = append(f.forgot, namespace+"/"+robotID)
	return true
}

// The whole point of the type: a Robot recreated under the same wire robot_id gets
// a new UID, and only that transition clears the projector cache. Steady-state
// reconciles of one object must not, or every requeue would re-establish status and
// turn a reconcile loop into a status-write loop (RA-1).
func TestRobotProjectionResetter_ForgetsOnlyOnAChangeOfObjectIdentity(t *testing.T) {
	f := &recordingForgetter{}
	r := &RobotProjectionResetter{}
	r.SetForgetter(f)

	const ns, robotID = "warehouse-a", "sim-robot-001"
	gen1, gen2 := types.UID("11111111-1111-1111-1111-111111111111"),
		types.UID("22222222-2222-2222-2222-222222222222")

	// First sight: the projector is empty too, so there is nothing to drop.
	if r.Observe(ns, robotID, gen1) {
		t.Fatal("first observation of a robot_id must not forget anything")
	}
	// Steady state: many reconciles of the same object.
	for i := 0; i < 25; i++ {
		if r.Observe(ns, robotID, gen1) {
			t.Fatalf("reconcile %d of an unchanged object must not forget anything", i)
		}
	}
	if len(f.forgot) != 0 {
		t.Fatalf("no forget expected yet, got %v", f.forgot)
	}

	// The Robot is deleted and recreated: same robot_id, new UID.
	if !r.Observe(ns, robotID, gen2) {
		t.Fatal("a new UID behind the same robot_id must drop the stale projector state")
	}
	if len(f.forgot) != 1 || f.forgot[0] != ns+"/"+robotID {
		t.Fatalf("expected exactly one forget of %s/%s, got %v", ns, robotID, f.forgot)
	}

	// And the new generation then settles: no repeat forgets.
	for i := 0; i < 10; i++ {
		if r.Observe(ns, robotID, gen2) {
			t.Fatalf("reconcile %d of the new generation must not forget again", i)
		}
	}
	if len(f.forgot) != 1 {
		t.Fatalf("expected the forget to happen once, got %v", f.forgot)
	}
}

// Two live Robots in two namespaces carry the same robot_id and, being two objects,
// two UIDs. Reconciles alternating between them are not a change of identity for
// either binding, so nothing is forgotten; a recreation in one namespace forgets
// that namespace's robot only.
func TestRobotProjectionResetter_SameRobotIDInTwoNamespacesIsTwoBindings(t *testing.T) {
	f := &recordingForgetter{}
	r := &RobotProjectionResetter{}
	r.SetForgetter(f)

	const robotID = "chassis-7"
	uidA, uidB := types.UID("aaaaaaaa-0000-0000-0000-000000000001"), types.UID("bbbbbbbb-0000-0000-0000-000000000002")
	for i := 0; i < 25; i++ {
		if r.Observe("staging", robotID, uidA) || r.Observe("production", robotID, uidB) {
			t.Fatalf("round %d: alternating reconciles of two live robots forgot projector state: %v", i, f.forgot)
		}
	}
	if len(f.forgot) != 0 {
		t.Fatalf("no forget expected, got %v", f.forgot)
	}

	if !r.Observe("staging", robotID, "aaaaaaaa-0000-0000-0000-000000000003") {
		t.Fatal("recreating the staging robot must drop its projector state")
	}
	if len(f.forgot) != 1 || f.forgot[0] != "staging/"+robotID {
		t.Fatalf("expected one forget of staging/%s only, got %v", robotID, f.forgot)
	}
}

// Robots that project no telemetry, and a manager with no ControlStream, must both
// be handled without a panic and without a spurious reset.
func TestRobotProjectionResetter_IgnoresUnusableInputAndStaysInertWithoutAForgetter(t *testing.T) {
	f := &recordingForgetter{}
	r := &RobotProjectionResetter{}
	r.SetForgetter(f)

	// A Robot with no swarmada.io/robot-id annotation never reaches RobotStatusSink
	// in the first place, so there is no projector state keyed to it.
	if r.Observe("warehouse-a", "", types.UID("11111111-1111-1111-1111-111111111111")) {
		t.Fatal("an empty robot_id must be ignored")
	}
	if r.Observe("warehouse-a", "sim-robot-001", "") {
		t.Fatal("an empty UID must be ignored")
	}
	if len(f.forgot) != 0 {
		t.Fatalf("expected no forgets, got %v", f.forgot)
	}

	// No ControlStream configured: no projector exists, so the resetter is inert.
	inert := &RobotProjectionResetter{}
	if inert.Observe("warehouse-a", "sim-robot-001", "aaa") || inert.Observe("warehouse-a", "sim-robot-001", "bbb") {
		t.Fatal("a resetter with no forgetter must report no reset")
	}

	// A nil resetter is the documented "disabled" case and must not panic: the Robot
	// reconciler calls Observe unconditionally.
	var nilResetter *RobotProjectionResetter
	if nilResetter.Observe("warehouse-a", "sim-robot-001", "ccc") {
		t.Fatal("a nil resetter must report no reset")
	}
}

// The wiring assertion: the Robot reconciler actually feeds the resetter, so a
// Robot recreated under the same swarmada.io/robot-id clears the projector cache
// the next time it is reconciled. This is the control-plane half of the fix, and
// the half that is easy to leave out: Forget and ForgetRobot can both be correct
// while nothing ever calls them. Without this wiring the recreated Robot's
// status.batteryPercent is never re-established for as long as the manager process
// runs, because the projector's cache still describes the previous object.
func TestRobotReconciler_ResetsProjectionWhenARobotIsRecreated(t *testing.T) {
	scheme := livenessScheme(t)
	f := &recordingForgetter{}
	resetter := &RobotProjectionResetter{}
	resetter.SetForgetter(f)

	robotOfGeneration := func(uid types.UID) *fleetv1.Robot {
		return &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sim-robot-001",
				Namespace:   "warehouse-a",
				UID:         uid,
				Annotations: map[string]string{RobotIDAnnotation: "sim-robot-001"},
			},
		}
	}
	reconcile := func(t *testing.T, robot *fleetv1.Robot) {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
			WithStatusSubresource(&fleetv1.Robot{}).Build()
		rr := &RobotReconciler{Client: c, Scheme: scheme, Projection: resetter}
		if _, err := rr.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: robot.Name, Namespace: robot.Namespace},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	gen1 := robotOfGeneration("11111111-1111-1111-1111-111111111111")
	reconcile(t, gen1)
	reconcile(t, gen1)
	if len(f.forgot) != 0 {
		t.Fatalf("reconciling one Robot generation must not reset its projection, got %v", f.forgot)
	}

	// Deleted and re-applied: same name, same wire robot_id, new UID.
	reconcile(t, robotOfGeneration("22222222-2222-2222-2222-222222222222"))
	if len(f.forgot) != 1 || f.forgot[0] != "warehouse-a/sim-robot-001" {
		t.Fatalf("expected exactly one projection reset for warehouse-a/sim-robot-001, got %v", f.forgot)
	}
}
