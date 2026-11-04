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
// THE ESTOP IDEMPOTENCE MARKER IS THE CONTROLLER'S TO WRITE — AT BIRTH TOO.
//
// safety_admission_envtest_test.go proves that a non-controller UPDATE writing
// swarmada.io/estop-processed is refused over the wire. That leaves CREATE, and the
// consequence is the same one: the zone controller reads a marker equal to the
// incoming trigger as "already fanned out" and returns without acting, so a carrier
// born with the marker already stamped swallows its first estop — nothing delivered
// and nothing audited. The default trigger value is "true", which is what the CLI
// writes when an operator gives no reason, so the value to pre-stamp is not a guess.
// Anyone who can create the object can do it, and anyone who can delete one can do it
// to an existing object by recreating it.
//
// So this asks the API server the create question directly, on all three carriers, and
// keeps three controls beside it: the same create WITHOUT the marker is admitted (the
// refusal is the marker, not the create), and the manager's own ServiceAccount may
// still create a carrier carrying it (the guard is an identity check, not a wall).
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// The Robot fixture's supporting objects. A Robot create over the wire is refused by
// the schema without manufacturer/model/zone/adapter, and by the admission gate
// unless spec.zone names a leaf FleetZone and spec.adapter names a Connected,
// conformance-Passed adapter qualified against a contract version this build drives.
// Without all of that, a "refused" Robot create would prove nothing about the marker.
const (
	markerRobotZone    = "bay-1"
	markerRobotAdapter = "acme"
)

// markerFixture creates the zone and adapter a schema-valid Robot needs in ns.
func markerFixture(t *testing.T, w *safetyWire, ns string) {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: markerRobotZone, Namespace: ns},
	}); err != nil {
		t.Fatalf("creating the robot's zone: %v", err)
	}
	adapter := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: markerRobotAdapter, Namespace: ns},
		Spec:       fleetv1.FleetAdapterSpec{Vendor: "acme-robotics"},
	}
	if err := w.admin.Create(ctx, adapter); err != nil {
		t.Fatalf("creating the FleetAdapter: %v", err)
	}
	// Status is a subresource, so the eligibility the gate reads is written after the
	// create. servesRobotClasses is left empty: a serves-any adapter matches the
	// classless Robot below, which keeps a RobotClass out of this fixture.
	adapter.Status = fleetv1.FleetAdapterStatus{
		Phase:                      fleetv1.FleetAdapterPhaseConnected,
		Conformance:                fleetv1.ConformanceStatePassed,
		ConformanceContractVersion: contract.Version,
	}
	if err := w.admin.Status().Update(ctx, adapter); err != nil {
		t.Fatalf("marking the FleetAdapter Connected and conformance-Passed: %v", err)
	}
}

// markerRobot builds a Robot that admission accepts, carrying ann.
func markerRobot(ns, name string, ann map[string]string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: ann},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "acme-robotics",
			Model:        "acme-picker",
			Zone:         markerRobotZone,
			Adapter:      fleetv1.AdapterRef{Name: markerRobotAdapter, Version: "1.0.0"},
		},
	}
}

// freshNamespace is for the SwarmadaConfig cases: the CRD's CEL pins the name to
// swarmada-config, so one namespace holds one of them and the three cases cannot
// share w.ns without depending on each other's order.
func freshNamespace(t *testing.T, w *safetyWire) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "marker-"}}
	if err := w.admin.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating a namespace: %v", err)
	}
	return ns.Name
}

// marked is the forgery: the value the next default-reason trigger will carry.
func marked() map[string]string {
	return map[string]string{estopProcessedAnnotation: "true"}
}

// namesTheMarker fails the subtest when a refusal does not say what was refused.
func namesTheMarker(t *testing.T, err error) {
	t.Helper()
	if !strings.Contains(err.Error(), estopProcessedAnnotation) {
		t.Errorf("refused, but the message does not name the marker: %v", err)
	}
}

func TestEnvtest_EstopProcessedMarkerOnCreateOverTheWire(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	mallory := w.as(t, "mallory")
	manager := w.as(t, controllerUser)
	markerFixture(t, w, w.ns)

	t.Run("ClientFleetZoneCreateCarryingMarkerRefused", func(t *testing.T) {
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
			Name: "dock", Namespace: w.ns,
			Annotations: map[string]string{estopProcessedAnnotation: "true"},
		}}
		err := mallory.Create(ctx, z)
		if err == nil || !apierrors.IsForbidden(err) {
			got := &fleetv1.FleetZone{}
			_ = w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "dock"}, got)
			t.Fatalf("a non-controller CREATE carrying %s was not refused (err=%v); stored marker=%q",
				estopProcessedAnnotation, err, got.Annotations[estopProcessedAnnotation])
		}
		namesTheMarker(t, err)
	})

	t.Run("ClientFleetZoneCreateWithoutMarkerAdmitted", func(t *testing.T) {
		// The control: the same principal creating the same kind of zone, marker
		// absent, is admitted — so the refusal above is the marker and not the create.
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "yard", Namespace: w.ns}}
		if err := mallory.Create(ctx, z); err != nil {
			t.Fatalf("a zone create with no marker must still be admitted, got: %v", err)
		}
	})

	t.Run("ControllerFleetZoneCreateCarryingMarkerAdmitted", func(t *testing.T) {
		// The exemption, over the wire as the API server authenticates it.
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
			Name: "annex", Namespace: w.ns, Annotations: marked(),
		}}
		if err := manager.Create(ctx, z); err != nil {
			t.Fatalf("the control plane's own zone create carrying the marker was refused: %v", err)
		}
		got := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(z), got); err != nil {
			t.Fatalf("re-reading the zone: %v", err)
		}
		if got.Annotations[estopProcessedAnnotation] != "true" {
			t.Fatalf("marker = %q after the control plane's create, want \"true\"",
				got.Annotations[estopProcessedAnnotation])
		}
	})

	t.Run("ClientSwarmadaConfigCreateCarryingMarkerRefused", func(t *testing.T) {
		ns := freshNamespace(t, w)
		cfg := signingConfig(ns, false)
		cfg.Annotations = marked()
		err := mallory.Create(ctx, cfg)
		if err == nil || !apierrors.IsForbidden(err) {
			got := &fleetv1.SwarmadaConfig{}
			_ = w.admin.Get(ctx, client.ObjectKeyFromObject(cfg), got)
			t.Fatalf("a non-controller CREATE of a SwarmadaConfig carrying %s was not refused "+
				"(err=%v); stored marker=%q", estopProcessedAnnotation, err,
				got.Annotations[estopProcessedAnnotation])
		}
		namesTheMarker(t, err)
	})

	t.Run("ClientSwarmadaConfigCreateWithoutMarkerAdmitted", func(t *testing.T) {
		if err := mallory.Create(ctx, signingConfig(freshNamespace(t, w), false)); err != nil {
			t.Fatalf("a SwarmadaConfig create with no marker must still be admitted, got: %v", err)
		}
	})

	t.Run("ControllerSwarmadaConfigCreateCarryingMarkerAdmitted", func(t *testing.T) {
		cfg := signingConfig(freshNamespace(t, w), false)
		cfg.Annotations = marked()
		if err := manager.Create(ctx, cfg); err != nil {
			t.Fatalf("the control plane's own SwarmadaConfig create carrying the marker was refused: %v", err)
		}
	})

	t.Run("ClientRobotCreateCarryingMarkerRefused", func(t *testing.T) {
		r := markerRobot(w.ns, "amr-1", marked())
		err := mallory.Create(ctx, r)
		if err == nil || !apierrors.IsForbidden(err) {
			got := &fleetv1.Robot{}
			_ = w.admin.Get(ctx, client.ObjectKeyFromObject(r), got)
			t.Fatalf("a non-controller CREATE of a Robot carrying %s was not refused (err=%v); "+
				"stored marker=%q", estopProcessedAnnotation, err,
				got.Annotations[estopProcessedAnnotation])
		}
		namesTheMarker(t, err)
	})

	t.Run("ClientRobotCreateWithoutMarkerAdmitted", func(t *testing.T) {
		// This control earns its place twice over: it is also what proves the Robot
		// fixture is admissible, so the refusal above cannot be the adapter gate or
		// the schema wearing the marker guard's clothes.
		if err := mallory.Create(ctx, markerRobot(w.ns, "amr-2", nil)); err != nil {
			t.Fatalf("a Robot create with no marker must still be admitted, got: %v", err)
		}
	})

	t.Run("ControllerRobotCreateCarryingMarkerAdmitted", func(t *testing.T) {
		if err := manager.Create(ctx, markerRobot(w.ns, "amr-3", marked())); err != nil {
			t.Fatalf("the control plane's own Robot create carrying the marker was refused: %v", err)
		}
	})
}
