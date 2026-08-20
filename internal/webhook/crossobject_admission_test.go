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

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// Cross-object admission rules (RFC-0001 §9.3.1) that single-object CEL cannot express,
// because each needs to read a DIFFERENT object than the one being admitted.
//
// Every rule is tested three ways: it rejects the invalid case, it admits the valid one,
// and it fails CLOSED when the lookup errors. The third is the one that matters and the
// one that is easy to omit — a validator that admits on a read error turns an API-server
// blip into an open gate, and no rejection-only test would notice.

const xoNS = "warehouse-a"

func xoScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// xoClientFailing builds a client whose FleetZone Get and/or Robot List fails, standing in
// for an API-server error. errors.New (not apierrors.NewNotFound) is deliberate: NotFound is
// a definite answer, anything else is "we do not know", and only the latter must fail closed.
func xoClientFailing(t *testing.T, failGet, failList bool, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(xoScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if failGet {
					if _, isZone := obj.(*fleetv1.FleetZone); isZone {
						return errors.New("etcd unavailable")
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if failList {
					if _, isRobots := list.(*fleetv1.RobotList); isRobots {
						return errors.New("etcd unavailable")
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
}

func xoClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(xoScheme(t)).WithObjects(objs...).Build()
}

func xoZone(name, parent string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: xoNS, Name: name},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
	}
}

// A FleetAdapter that satisfies the pre-existing adapter gate, so these tests fail for the
// rule under test rather than for an unrelated binding.
func xoAdapter() *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Namespace: xoNS, Name: "acme-adapter"},
		Spec:       fleetv1.FleetAdapterSpec{Endpoint: "adapter.svc:9443"},
		Status: fleetv1.FleetAdapterStatus{
			Phase:       fleetv1.FleetAdapterPhaseConnected,
			Conformance: fleetv1.ConformanceStatePassed,
			// The pre-existing ADR-0032 gate also requires the conformance result to be
			// bound to a supported contract version; without it these fixtures would fail
			// for a reason unrelated to the rules under test.
			ConformanceContractVersion: contract.Version,
		},
	}
}

func xoRobot(name, zone, robotID string) *fleetv1.Robot {
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: xoNS, Name: name},
		Spec: fleetv1.RobotSpec{
			Zone:         zone,
			Manufacturer: "acme",
			Model:        "amr-100",
			Adapter:      fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
		},
	}
	if robotID != "" {
		r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: robotID}
	}
	return r
}

func mustRejectWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("admitted; expected rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejected for the wrong reason.\n  want substring: %s\n  got: %v", want, err)
	}
}

// ── Robot.spec.zone must be a leaf FleetZone ───────────────────────────────────────────

func TestRobotAdmission_ZoneMustBeLeaf(t *testing.T) {
	ctx := context.Background()
	// floor-1 has a child (aisle-b3), so it is a parent; aisle-b3 is a leaf.
	objs := []client.Object{xoAdapter(), xoZone("floor-1", ""), xoZone("aisle-b3", "floor-1")}

	g := &RobotAdmissionGate{Client: xoClient(t, objs...)}
	_, err := g.ValidateCreate(ctx, xoRobot("amr-1", "floor-1", "rid-1"))
	mustRejectWith(t, err, `spec.zone "floor-1" is not a leaf zone (has children: [aisle-b3])`)

	if _, err := g.ValidateCreate(ctx, xoRobot("amr-2", "aisle-b3", "rid-2")); err != nil {
		t.Fatalf("a robot in a leaf zone must be admitted: %v", err)
	}
}

func TestRobotAdmission_ZoneMustExist(t *testing.T) {
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""))}
	_, err := g.ValidateCreate(context.Background(), xoRobot("amr-1", "nowhere", "rid-1"))
	mustRejectWith(t, err, `FleetZone "nowhere" not found`)
}

func TestRobotAdmission_ZoneLookupErrorFailsClosed(t *testing.T) {
	// THE CASE THAT MATTERS. An unreadable zone is not an absent one. Admitting here would
	// place robots in unverified zones for exactly as long as the API server is unhealthy.
	c := xoClientFailing(t, true, false, xoAdapter(), xoZone("aisle-b3", ""))
	g := &RobotAdmissionGate{Client: c}
	_, err := g.ValidateCreate(context.Background(), xoRobot("amr-1", "aisle-b3", "rid-1"))
	if err == nil {
		t.Fatal("admitted despite an unreadable FleetZone; the gate must fail closed")
	}
	if !strings.Contains(err.Error(), "etcd unavailable") {
		t.Fatalf("expected the underlying read error to surface, got: %v", err)
	}
}

// ── swarmada.io/robot-id must be unique in the namespace ───────────────────────────────

func TestRobotAdmission_RobotIDIsUniqueInNamespace(t *testing.T) {
	ctx := context.Background()
	existing := xoRobot("amr-1", "aisle-b3", "rid-shared")
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""), existing)}

	_, err := g.ValidateCreate(ctx, xoRobot("amr-2", "aisle-b3", "rid-shared"))
	mustRejectWith(t, err, `robot-id "rid-shared" is already bound to Robot "amr-1"`)

	if _, err := g.ValidateCreate(ctx, xoRobot("amr-2", "aisle-b3", "rid-distinct")); err != nil {
		t.Fatalf("a distinct robot-id must be admitted: %v", err)
	}
}

func TestRobotAdmission_RobotIDDoesNotCollideWithItself(t *testing.T) {
	// An update re-sends the same object. Comparing on the annotation alone — without
	// excluding the object's own name — would make every Robot un-updatable the moment it
	// carried a robot-id, which is to say: always.
	//
	// This exercises the rule for real only since the uniqueness check moved out of the
	// update short-circuit: the update below changes neither class nor adapter, so the
	// gate previously returned before the robot-id loop ever ran.
	ctx := context.Background()
	existing := xoRobot("amr-1", "aisle-b3", "rid-1")
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""), existing)}

	updated := xoRobot("amr-1", "aisle-b3", "rid-1")
	updated.Spec.Model = "amr-200"
	if _, err := g.ValidateUpdate(ctx, existing, updated); err != nil {
		t.Fatalf("a Robot must not collide with its own robot-id on update: %v", err)
	}
}

func TestRobotAdmission_RobotIDCollisionIntroducedByUpdateIsRejected(t *testing.T) {
	// THE CASE THE UPDATE SHORT-CIRCUIT USED TO MISS. Uniqueness was asserted at create
	// only, so `kubectl annotate robot amr-2 swarmada.io/robot-id=rid-1 --overwrite` handed
	// a second Robot the identity the Scheduler dispatches by (§9.1.2.6) — two tasks, one
	// physical robot, which is exactly what the rule exists to prevent. The update below
	// changes neither spec.robotClass nor spec.adapter.name, so it is the shape of write
	// the old gate waved through.
	ctx := context.Background()
	existing := xoRobot("amr-1", "aisle-b3", "rid-1")
	current := xoRobot("amr-2", "aisle-b3", "rid-2")
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""), existing, current)}

	stolen := xoRobot("amr-2", "aisle-b3", "rid-1")
	_, err := g.ValidateUpdate(ctx, current, stolen)
	mustRejectWith(t, err, `robot-id "rid-1" is already bound to Robot "amr-1"`)
}

func TestRobotAdmission_RobotIDListErrorFailsClosed(t *testing.T) {
	c := xoClientFailing(t, false, true, xoAdapter(), xoZone("aisle-b3", ""))
	g := &RobotAdmissionGate{Client: c}
	_, err := g.ValidateCreate(context.Background(), xoRobot("amr-1", "aisle-b3", "rid-1"))
	if err == nil {
		t.Fatal("admitted despite an unreadable Robot list; uniqueness cannot be asserted, so it must fail closed")
	}
}

func TestRobotAdmission_NoRobotIDIsNotACollision(t *testing.T) {
	// Two robots with no annotation share the empty string. Treating that as a collision
	// would block the second operator-created Robot in every namespace.
	ctx := context.Background()
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""),
		xoRobot("amr-1", "aisle-b3", ""))}
	if _, err := g.ValidateCreate(ctx, xoRobot("amr-2", "aisle-b3", "")); err != nil {
		t.Fatalf("robots without a robot-id must not collide: %v", err)
	}
}

// ── ZoneMaintenance.spec.scope.zoneName must exist ─────────────────────────────────────

func xoMaintenance(name string, scope fleetv1.MaintenanceScopeType, zoneName string) *fleetv1.ZoneMaintenance {
	return &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Namespace: xoNS, Name: name},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: scope, ZoneName: zoneName},
		},
	}
}

func TestZoneMaintenanceAdmission_ZoneMustExist(t *testing.T) {
	ctx := context.Background()
	v := &ZoneMaintenanceValidator{Client: xoClient(t, xoZone("aisle-b3", ""))}

	_, err := v.ValidateCreate(ctx, xoMaintenance("zm-1", fleetv1.MaintenanceScopeZone, "nowhere"))
	mustRejectWith(t, err, `FleetZone "nowhere" does not exist in namespace "warehouse-a"`)

	if _, err := v.ValidateCreate(ctx, xoMaintenance("zm-2", fleetv1.MaintenanceScopeZone, "aisle-b3")); err != nil {
		t.Fatalf("an existing zone must be admitted: %v", err)
	}
}

func TestZoneMaintenanceAdmission_ParentZoneIsAllowed(t *testing.T) {
	// Unlike Robot.spec.zone, a Zone-scoped window resolves to the zone AND its descendants
	// (§9.1.10), so maintaining a whole branch is the intended use — not an error.
	ctx := context.Background()
	v := &ZoneMaintenanceValidator{
		Client: xoClient(t, xoZone("floor-1", ""), xoZone("aisle-b3", "floor-1")),
	}
	if _, err := v.ValidateCreate(ctx, xoMaintenance("zm-1", fleetv1.MaintenanceScopeZone, "floor-1")); err != nil {
		t.Fatalf("a parent zone must be admissible for maintenance: %v", err)
	}
}

func TestZoneMaintenanceAdmission_NamespaceScopeNeedsNoZone(t *testing.T) {
	ctx := context.Background()
	v := &ZoneMaintenanceValidator{Client: xoClient(t)}
	if _, err := v.ValidateCreate(ctx, xoMaintenance("zm-1", fleetv1.MaintenanceScopeNamespace, "")); err != nil {
		t.Fatalf("a Namespace-scoped window carries no zoneName to resolve: %v", err)
	}
}

func TestZoneMaintenanceAdmission_ZoneScopeRequiresAName(t *testing.T) {
	ctx := context.Background()
	v := &ZoneMaintenanceValidator{Client: xoClient(t)}
	_, err := v.ValidateCreate(ctx, xoMaintenance("zm-1", fleetv1.MaintenanceScopeZone, ""))
	mustRejectWith(t, err, "zoneName is required when scope.type is Zone")
}

func TestZoneMaintenanceAdmission_LookupErrorFailsClosed(t *testing.T) {
	// A window created during API-server trouble is precisely when an operator is reacting
	// to something. Admitting one that silently pauses nothing is the worst moment for it.
	c := xoClientFailing(t, true, false, xoZone("aisle-b3", ""))
	v := &ZoneMaintenanceValidator{Client: c}
	_, err := v.ValidateCreate(context.Background(),
		xoMaintenance("zm-1", fleetv1.MaintenanceScopeZone, "aisle-b3"))
	if err == nil {
		t.Fatal("admitted despite an unreadable FleetZone; the validator must fail closed")
	}
}

func TestZoneMaintenanceAdmission_DeleteIsAllowed(t *testing.T) {
	// Deletion resumes paused robots through the finalizer; refusing it would strand them.
	v := &ZoneMaintenanceValidator{Client: xoClient(t)}
	if _, err := v.ValidateDelete(context.Background(),
		xoMaintenance("zm-1", fleetv1.MaintenanceScopeZone, "gone")); err != nil {
		t.Fatalf("delete must be allowed even when the zone is gone: %v", err)
	}
}

// ── Robot.spec.charging.dockName must resolve in the zone or an ancestor ───────────────

func xoZoneWithDock(name, parent, dockName string, dockType fleetv1.SharedResourceType) *fleetv1.FleetZone {
	z := xoZone(name, parent)
	if dockName != "" {
		z.Spec.SharedResources = []fleetv1.SharedResource{{Name: dockName, Type: dockType, Capacity: 1}}
	}
	return z
}

func xoRobotWithDock(name, zone, dock string) *fleetv1.Robot {
	r := xoRobot(name, zone, "rid-"+name)
	r.Spec.Charging = &fleetv1.RobotChargingConfig{DockName: dock}
	return r
}

func TestRobotAdmission_DockMustExistInZoneOrAncestor(t *testing.T) {
	ctx := context.Background()
	// dock-1 is declared on the PARENT; aisle-b3 is the robot's leaf zone. Shared resources
	// are inherited downward, so this must resolve.
	objs := []client.Object{
		xoAdapter(),
		xoZoneWithDock("floor-1", "", "dock-1", fleetv1.SharedResourceChargingDock),
		xoZone("aisle-b3", "floor-1"),
	}
	g := &RobotAdmissionGate{Client: xoClient(t, objs...)}

	if _, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-1", "aisle-b3", "dock-1")); err != nil {
		t.Fatalf("a dock declared on an ancestor zone must resolve: %v", err)
	}
	_, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-2", "aisle-b3", "dock-nowhere"))
	mustRejectWith(t, err, `charging.dockName "dock-nowhere" is not a ChargingDock in zone "aisle-b3" or its ancestors`)
}

func TestRobotAdmission_DockInOwnZoneResolves(t *testing.T) {
	ctx := context.Background()
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(),
		xoZoneWithDock("aisle-b3", "", "dock-1", fleetv1.SharedResourceChargingDock))}
	if _, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-1", "aisle-b3", "dock-1")); err != nil {
		t.Fatalf("a dock in the robot's own zone must resolve: %v", err)
	}
}

func TestRobotAdmission_DockInASiblingBranchDoesNotResolve(t *testing.T) {
	// THE REASON THE WALK IS UPWARD ONLY. A dock in a sibling branch is inherited by that
	// branch, not this one — the robot cannot reach it, so naming it is a misconfiguration.
	ctx := context.Background()
	objs := []client.Object{
		xoAdapter(),
		xoZone("floor-1", ""),
		xoZone("aisle-b3", "floor-1"),
		xoZoneWithDock("aisle-c9", "floor-1", "dock-c", fleetv1.SharedResourceChargingDock),
	}
	g := &RobotAdmissionGate{Client: xoClient(t, objs...)}
	_, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-1", "aisle-b3", "dock-c"))
	mustRejectWith(t, err, `is not a ChargingDock in zone "aisle-b3" or its ancestors`)
}

func TestRobotAdmission_DockOfTheWrongTypeDoesNotResolve(t *testing.T) {
	// A resource of the right NAME but the wrong TYPE must not match: reserving a Corridor
	// as a charging dock would queue the robot behind something that never charges it.
	ctx := context.Background()
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(),
		xoZoneWithDock("aisle-b3", "", "dock-1", fleetv1.SharedResourceCorridor))}
	_, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-1", "aisle-b3", "dock-1"))
	mustRejectWith(t, err, `is not a ChargingDock`)
}

func TestRobotAdmission_NoDockNameIsNotChecked(t *testing.T) {
	// dockName is optional; a robot without one (or without a charging block at all) must
	// be admitted, or every robot that does not pin a dock becomes unadmittable.
	ctx := context.Background()
	g := &RobotAdmissionGate{Client: xoClient(t, xoAdapter(), xoZone("aisle-b3", ""))}
	if _, err := g.ValidateCreate(ctx, xoRobot("amr-1", "aisle-b3", "rid-1")); err != nil {
		t.Fatalf("no charging block must be admitted: %v", err)
	}
	if _, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-2", "aisle-b3", "")); err != nil {
		t.Fatalf("an empty dockName must be admitted: %v", err)
	}
}

func TestRobotAdmission_DockLookupErrorFailsClosed(t *testing.T) {
	// An unreadable ancestor might be the one declaring the dock, so "not found" is not a
	// safe conclusion — the gate must fail closed rather than reject a valid robot or admit
	// an invalid one on incomplete information.
	c := xoClientFailing(t, true, false,
		xoAdapter(), xoZoneWithDock("aisle-b3", "", "dock-1", fleetv1.SharedResourceChargingDock))
	g := &RobotAdmissionGate{Client: c}
	_, err := g.ValidateCreate(context.Background(), xoRobotWithDock("amr-1", "aisle-b3", "dock-1"))
	if err == nil {
		t.Fatal("admitted despite an unreadable zone; the gate must fail closed")
	}
}

func TestRobotAdmission_DockWalkIsDepthBounded(t *testing.T) {
	// A cyclic parent chain should be impossible — the FleetZone webhook rejects cycles at
	// zone admission — but this walk must not DEPEND on that, because a webhook that spins
	// stalls every Robot write in the namespace behind a request that never returns. The
	// FleetZone validator can be disabled, and etcd can hold objects written before it existed.
	ctx := context.Background()
	objs := []client.Object{
		xoAdapter(),
		// a → b → a, and the robot sits in `a`.
		xoZone("zone-a", "zone-b"),
		xoZone("zone-b", "zone-a"),
	}
	g := &RobotAdmissionGate{Client: xoClient(t, objs...)}

	done := make(chan error, 1)
	go func() {
		_, err := g.ValidateCreate(ctx, xoRobotWithDock("amr-1", "zone-a", "dock-1"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cyclic zone chain must not be admitted silently")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parentZone walk did not terminate; a spinning webhook blocks all Robot writes")
	}
}
