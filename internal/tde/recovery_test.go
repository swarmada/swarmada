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

package tde

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// zoneWithReservations builds a FleetZone whose status carries pre-crash entries.
func zoneWithReservations(name string, maxRobots int32, rs ...fleetv1.ZoneReservation) *fleetv1.FleetZone {
	z := fzone(name, maxRobots)
	z.Status.Reservations = rs
	return z
}

func reservation(actionID, robotID string, state fleetv1.ReservationState) fleetv1.ZoneReservation {
	return fleetv1.ZoneReservation{
		RobotID: robotID, ActionID: actionID, Priority: fleetv1.ActionPriorityNormal,
		State: state, GrantedAt: metav1.Now(),
	}
}

func robotInZone(name, zone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     fleetv1.RobotStatus{CurrentZone: zone},
	}
}

func recoveryClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetZone{}).Build()
}

func zoneReservations(t *testing.T, c client.Client, name string) []fleetv1.ZoneReservation {
	t.Helper()
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, z); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	return z.Status.Reservations
}

// Validate: keep an Occupied whose robot is still in the zone; drop a Reserved;
// drop an Occupied whose robot has left.
func TestRecover_Validate(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t-here", "r-here", fleetv1.ReservationOccupied),
			reservation("t-left", "r-left", fleetv1.ReservationOccupied),
			reservation("t-transit", "r-transit", fleetv1.ReservationReserved),
		),
		robotInZone("r-here", "z"),     // still present → keep
		robotInZone("r-left", "other"), // moved away → drop
		robotInZone("r-transit", "z"),  // Reserved is dropped regardless
	)
	e := New(c, DefaultConfig())

	if err := e.Recover(context.Background(), c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}

	rs := zoneReservations(t, c, "z")
	if len(rs) != 1 || rs[0].ActionID != "t-here" {
		t.Fatalf("recovered reservations = %+v, want only [t-here] (present Occupied)", rs)
	}
	// The recovered slot must count toward capacity for a subsequent request.
	st, _ := e.ZoneStatus(context.Background(), ns, "z")
	if st.Occupied != 1 {
		t.Fatalf("recovered Occupied count = %d, want 1", st.Occupied)
	}
}

func TestRecover_ReleaseAll(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t1", "r1", fleetv1.ReservationOccupied),
			reservation("t2", "r2", fleetv1.ReservationReserved),
		),
		robotInZone("r1", "z"),
	)
	e := New(c, DefaultConfig())

	if err := e.Recover(context.Background(), c, RecoverReleaseAll); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rs := zoneReservations(t, c, "z"); len(rs) != 0 {
		t.Fatalf("ReleaseAll left %+v, want none", rs)
	}
}

func TestRecover_ReleaseReservedOnly(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t-occ", "r-occ", fleetv1.ReservationOccupied),
			reservation("t-res", "r-res", fleetv1.ReservationReserved),
		),
		// No Robot objects needed: ReleaseReservedOnly keeps Occupied without validating.
	)
	e := New(c, DefaultConfig())

	if err := e.Recover(context.Background(), c, RecoverReleaseReservedOnly); err != nil {
		t.Fatalf("recover: %v", err)
	}
	rs := zoneReservations(t, c, "z")
	if len(rs) != 1 || rs[0].ActionID != "t-occ" {
		t.Fatalf("ReleaseReservedOnly = %+v, want only [t-occ]", rs)
	}
}

// After recovery, a full zone stays full: a new request is Denied (the surviving
// Occupied slot is counted — no over-grant against pre-crash state).
func TestRecover_ThenRequestRespectsRecoveredCapacity(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 1,
			reservation("t-occ", "r-occ", fleetv1.ReservationOccupied),
		),
		robotInZone("r-occ", "z"),
	)
	e := New(c, DefaultConfig())
	if err := e.Recover(context.Background(), c, RecoverValidate); err != nil {
		t.Fatalf("recover: %v", err)
	}

	res, _ := e.RequestReservation(context.Background(), req("newtask", fleetv1.ActionPriorityNormal))
	if res.Status != Denied {
		t.Fatalf("post-recovery request = %s, want Denied (recovered slot must count toward capacity)", res.Status)
	}
}
