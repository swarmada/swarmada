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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
)

// Operator admission completes HERE, not in the CLI (§9.1.2.5). `swarmctl admit` records the
// operator's parameters on the DiscoveredRobot; this controller builds the Robot and removes
// the staging object, so promotion from discovered to schedulable happens in one place for
// both the operator and auto-admit paths.

// marked builds a DiscoveredRobot carrying an admission mark, the way the CLI writes it.
func marked(t *testing.T, name string, p admission.Params) *fleetv1.DiscoveredRobot {
	t.Helper()
	dr := discovered(name, time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered)
	dr.Status.Manufacturer = "Acme"
	dr.Status.AdapterVersion = "1.0"
	encoded, err := p.Encode()
	if err != nil {
		t.Fatalf("encoding params: %v", err)
	}
	dr.Annotations = map[string]string{admission.AdmitAnnotation: encoded}
	return dr
}

func admitClass(name string) *fleetv1.RobotClass {
	return &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: name},
		Spec: fleetv1.RobotClassSpec{
			BaseAdapter: fleetv1.BaseAdapterRef{Name: "acme-adapter", Version: "2.0"},
			Hardware:    []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}},
		},
	}
}

// admittedRobot fetches a Robot in the DiscoveredRobot test namespace, nil when absent.
func admittedRobot(t *testing.T, c client.Client, name string) *fleetv1.Robot {
	t.Helper()
	var robot fleetv1.Robot
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: drNS, Name: name}, &robot); err != nil {
		return nil
	}
	return &robot
}

func TestOperatorAdmit_CreatesTheRobotThenRemovesTheStagingObject(t *testing.T) {
	now := drBase
	p := admission.Params{Zone: "zone-aisle-c1", RobotClass: "acme-class", Name: "amr-042"}
	r, c := newDRReconciler(t, &now, marked(t, "dr-1", p), admitClass("acme-class"))

	reconcileDR(t, r, "dr-1")

	robot := admittedRobot(t, c, "amr-042")
	if robot == nil {
		t.Fatal("the controller must create the Robot the operator admitted")
	}
	if robot.Spec.Zone != "zone-aisle-c1" || robot.Spec.RobotClass != "acme-class" {
		t.Errorf("the operator's parameters did not reach the Robot: %+v", robot.Spec)
	}
	// Built through the shared builder, so the class template applies exactly as it does on
	// the CLI-validated preview.
	if robot.Spec.Adapter.Version != "2.0" || len(robot.Spec.Hardware) != 1 {
		t.Errorf("class template not applied: %+v", robot.Spec)
	}
	// The Robot exists first, then the staging object goes. A DiscoveredRobot deleted before
	// a failed create would lose the robot entirely.
	if dr := getDROrNil(t, c, "dr-1"); dr != nil {
		t.Error("the staging object must be removed once the Robot exists")
	}
}

func TestOperatorAdmit_IsNotMarkedAsAutoAdmitted(t *testing.T) {
	// ROBOT_ADMITTED records which gate the robot passed. An operator admission that looked
	// like a zero-touch one would misreport a reviewed decision as an automatic one.
	now := drBase
	r, c := newDRReconciler(t, &now, marked(t, "dr-1", admission.Params{Zone: "z", Adapter: "a"}))

	reconcileDR(t, r, "dr-1")
	robot := admittedRobot(t, c, "dr-1")
	if robot == nil {
		t.Fatal("robot not created")
	}
	if robot.Annotations[fleetv1.AutoAdmittedAnnotation] == "true" {
		t.Error("an operator admission must not carry the auto-admit annotation")
	}
}

func TestOperatorAdmit_ExistingRobotStillClearsTheStagingObject(t *testing.T) {
	// THE ORPHAN CASE. A previous attempt created the Robot and died before the delete.
	// Re-reconciling must finish the job rather than wedging on AlreadyExists — that crash
	// window is exactly what the old CLI-side create-then-delete could only warn about.
	now := drBase
	existing := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: "amr-042"},
		Spec: fleetv1.RobotSpec{
			Zone: "zone-aisle-c1", Manufacturer: "Acme",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "2.0"},
		},
	}
	p := admission.Params{Zone: "zone-aisle-c1", Adapter: "acme-adapter", Name: "amr-042"}
	r, c := newDRReconciler(t, &now, marked(t, "dr-1", p), existing)

	reconcileDR(t, r, "dr-1")
	if dr := getDROrNil(t, c, "dr-1"); dr != nil {
		t.Fatal("an already-created Robot must still clear the staging object")
	}
}

func TestOperatorAdmit_MissingClassRetriesAndKeepsTheObject(t *testing.T) {
	// The CLI verified the class existed when the operator admitted, so its absence now is a
	// race. Retrying is right; admitting without the template would silently produce a robot
	// missing the capabilities the class was supposed to grant.
	now := drBase
	p := admission.Params{Zone: "z", RobotClass: "gone-class"}
	r, c := newDRReconciler(t, &now, marked(t, "dr-1", p))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: drNS, Name: "dr-1"},
	})
	if err == nil {
		t.Fatal("a missing class must surface as a retryable error, not a silent skip")
	}
	if dr := getDROrNil(t, c, "dr-1"); dr == nil {
		t.Fatal("the staging object must survive so the admission can still complete")
	}
	if admittedRobot(t, c, "dr-1") != nil {
		t.Error("no Robot may be created without the class the operator named")
	}
}

func TestOperatorAdmit_UnusableMarkFallsThroughToTheSweep(t *testing.T) {
	// A corrupt payload cannot be acted on and will not decode differently next time. It
	// must not pin the object either: an admission branch that swallowed the reconcile would
	// leave the DiscoveredRobot in the namespace forever, exempt from the TTL it was
	// created with.
	now := drBase
	dr := discovered("dr-1", time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered)
	dr.Annotations = map[string]string{admission.AdmitAnnotation: "{not json"}
	r, c := newDRReconciler(t, &now, dr)

	now = drBase.Add(2 * time.Hour) // past the TTL
	reconcileDR(t, r, "dr-1")

	if dr := getDROrNil(t, c, "dr-1"); dr != nil {
		t.Fatal("an unusable mark must not exempt the object from the TTL sweep")
	}
	if admittedRobot(t, c, "dr-1") != nil {
		t.Error("a mark that does not decode must never produce a Robot")
	}
}

func TestOperatorAdmit_RejectionOutranksAnAdmissionMark(t *testing.T) {
	// Both marks present is a contradiction — two operators, or a mind changed. Refusing is
	// the fail-safe reading: an unwanted robot that is not admitted can be admitted later,
	// but one admitted against a rejection is already schedulable.
	now := drBase
	rec := &recordingAudit{}
	dr := marked(t, "dr-1", admission.Params{Zone: "z", Adapter: "a"})
	dr.Annotations[annRobotRejected] = "failed inspection"
	r, c := newDRReconciler(t, &now, dr)
	r.Audit = rec

	reconcileDR(t, r, "dr-1")
	if admittedRobot(t, c, "dr-1") != nil {
		t.Fatal("a rejected robot must not be admitted")
	}
	if n := len(rec.ofType("ROBOT_REJECTED")); n != 1 {
		t.Errorf("the rejection must still be sealed, got %d entries", n)
	}
}

func TestOperatorAdmit_UnmarkedObjectIsUntouched(t *testing.T) {
	// The branch is annotation-gated: an ordinary discovered robot awaiting a decision must
	// keep waiting, not be promoted by default.
	now := drBase
	r, c := newDRReconciler(t, &now, discovered("dr-1", time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered))

	reconcileDR(t, r, "dr-1")
	if admittedRobot(t, c, "dr-1") != nil {
		t.Fatal("an unmarked DiscoveredRobot must not be admitted")
	}
	if dr := getDROrNil(t, c, "dr-1"); dr == nil {
		t.Fatal("an unmarked DiscoveredRobot must survive until its TTL")
	}
}
