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
// AN ESTOP ON A COMPOSITE ZONE IS REFUSED, NOT SILENTLY ACCEPTED (FZ-C5).
//
// A composite FleetZone groups other zones through spec.memberZones. Composition
// asserts nothing physical, so estop propagation follows containment edges only
// (FZ-C4) and an estop on a composite reaches no robot: the composite holds none
// directly and propagates to none of its members. RFC-0001 §9.1.6.8 therefore requires
// the estop path to REJECT the trigger at admission, naming the members, rather than
// accept a stop that stops nothing.
//
// This sends the trigger over the wire, as an operator permitted the estop verb, and
// — if admission lets it through — runs the shipped zone-estop controller against the
// same API server to show which robots the accepted stop actually reached.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// recordingEstopper is shared with estop_marker_create_fanout_envtest_test.go.

func TestEnvtest_EstopOnCompositeZoneIsRefusedAtAdmission(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)

	for _, z := range []*fleetv1.FleetZone{
		{ObjectMeta: metav1.ObjectMeta{Name: "room-a", Namespace: w.ns}},
		{ObjectMeta: metav1.ObjectMeta{Name: "room-b", Namespace: w.ns}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ward-2", Namespace: w.ns},
			Spec: fleetv1.FleetZoneSpec{MemberZones: []string{"room-a", "room-b"}}},
	} {
		if err := w.admin.Create(ctx, z); err != nil {
			t.Fatalf("creating zone %s: %v", z.Name, err)
		}
	}
	robot := markerRobot(w.ns, "r-a", nil)
	robot.Spec.Zone = "room-a"
	if err := w.admin.Create(ctx, robot); err != nil {
		t.Fatalf("creating a robot in member zone room-a: %v", err)
	}

	operator := w.as(t, "ops-lead")
	trigger := func(name string) error {
		z := &fleetv1.FleetZone{}
		if err := operator.Get(ctx, types.NamespacedName{Namespace: w.ns, Name: name}, z); err != nil {
			t.Fatalf("get zone %s: %v", name, err)
		}
		base := z.DeepCopy()
		if z.Annotations == nil {
			z.Annotations = map[string]string{}
		}
		z.Annotations[estopTriggeredAnnotation] = "smoke in the ward"
		return operator.Patch(ctx, z, client.MergeFrom(base))
	}

	err := trigger("ward-2")
	if err == nil {
		// Admission accepted it. Show what the accepted stop did.
		est := &recordingEstopper{}
		r := &controller.ZoneEstopReconciler{Client: w.as(t, controllerUser), Estopper: est}
		if _, rerr := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: w.ns, Name: "ward-2"},
		}); rerr != nil {
			t.Fatalf("zone estop reconcile: %v", rerr)
		}
		t.Fatalf("an estop on composite zone ward-2 was ADMITTED. The zone-estop controller then "+
			"stopped %d robot(s) %v; robot r-a in member zone room-a was not stopped. An operator "+
			"who stops the ward believes its robots are stopped and none are. FZ-C5 requires the "+
			"trigger to be refused at admission, naming the members.", len(est.delivered()), est.delivered())
	}
	msg := err.Error()
	for _, want := range []string{
		"FleetZone 'ward-2' is a composite zone; estop does not propagate across composition edges",
		"room-a", "room-b", "namespace scope",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q; got: %v", want, err)
		}
	}

	// Control: the refusal is about composition, not the trigger. The same operator may
	// stop a member directly.
	if err := trigger("room-a"); err != nil {
		t.Fatalf("an estop on terminal member room-a was refused; only a composite target "+
			"should be: %v", err)
	}
}
