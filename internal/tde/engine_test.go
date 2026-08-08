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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const ns = "warehouse-a"

func newEngine(t *testing.T, zones ...*fleetv1.FleetZone) (*Engine, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	objs := make([]client.Object, len(zones))
	for i, z := range zones {
		objs[i] = z
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetZone{}).Build()
	// These tests exercise post-recovery grant logic; mark the engine recovered so
	// the fail-closed gate is open (production recovers via Recover/RecoveryRunnable).
	e := New(c, DefaultConfig())
	e.SetRecovered(true)
	return e, c
}

func fzone(name string, maxRobots int32, resources ...fleetv1.SharedResource) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetZoneSpec{MaxConcurrentRobots: maxRobots, SharedResources: resources},
	}
}

func req(actionID string, prio fleetv1.ActionPriority) ReservationRequest {
	return ReservationRequest{
		ActionID: actionID, RobotID: "robot-" + actionID, Namespace: ns,
		TargetZone: "z", Priority: prio,
	}
}

func TestRequestReservation_CapacityAndDeny(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 2))
	ctx := context.Background()

	for _, id := range []string{"t1", "t2"} {
		res, err := e.RequestReservation(ctx, req(id, fleetv1.ActionPriorityNormal))
		if err != nil || res.Status != Granted {
			t.Fatalf("%s: status=%s err=%v, want Granted", id, res.Status, err)
		}
	}
	res, _ := e.RequestReservation(ctx, req("t3", fleetv1.ActionPriorityNormal))
	if res.Status != Denied || res.DeniedReason != DeniedZoneCapacity {
		t.Fatalf("t3: status=%s reason=%s, want Denied/zone_capacity", res.Status, res.DeniedReason)
	}
}

func TestRequestReservation_NoLimit(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 0))
	for i := 0; i < 50; i++ {
		res, _ := e.RequestReservation(context.Background(), req(fmt.Sprintf("t%d", i), fleetv1.ActionPriorityNormal))
		if res.Status != Granted {
			t.Fatalf("no-limit zone must always grant, got %s", res.Status)
		}
	}
}

func TestRequestReservation_CriticalPreempts(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 1))
	ctx := context.Background()

	if res, _ := e.RequestReservation(ctx, req("normal", fleetv1.ActionPriorityNormal)); res.Status != Granted {
		t.Fatalf("normal grant: %s", res.Status)
	}
	// Zone full; a Critical action preempts the Normal reservation.
	res, _ := e.RequestReservation(ctx, req("crit", fleetv1.ActionPriorityCritical))
	if res.Status != PreemptedGranted {
		t.Fatalf("critical: status=%s, want PreemptedGranted", res.Status)
	}
	if len(res.PreemptedActionIDs) != 1 || res.PreemptedActionIDs[0] != "normal" {
		t.Fatalf("preempted = %v, want [normal]", res.PreemptedActionIDs)
	}
}

func TestRequestReservation_CriticalDoesNotPreemptCritical(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 1))
	ctx := context.Background()
	_, _ = e.RequestReservation(ctx, req("crit1", fleetv1.ActionPriorityCritical))
	res, _ := e.RequestReservation(ctx, req("crit2", fleetv1.ActionPriorityCritical))
	if res.Status != Denied {
		t.Fatalf("critical must not preempt another critical (FIFO): got %s", res.Status)
	}
}

// Critical does NOT preempt a High reservation — same band rule as the §C
// controller preemption (only Normal/Low), stricter than §9.4.6's "any non-Critical".
func TestRequestReservation_DoesNotPreemptHigh(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 1))
	ctx := context.Background()
	if res, _ := e.RequestReservation(ctx, req("high", fleetv1.ActionPriorityHigh)); res.Status != Granted {
		t.Fatalf("high grant: %s", res.Status)
	}
	res, _ := e.RequestReservation(ctx, req("crit", fleetv1.ActionPriorityCritical))
	if res.Status != Denied {
		t.Fatalf("Critical preempted a High reservation (%s); must be Denied (Normal/Low only)", res.Status)
	}
}

// §9.4.6 High-band preemption: a High action preempts a Normal reservation in a full
// zone — same path as Critical (both are preemptor bands).
func TestRequestReservation_HighPreemptsNormal(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 1))
	ctx := context.Background()
	if res, _ := e.RequestReservation(ctx, req("normal", fleetv1.ActionPriorityNormal)); res.Status != Granted {
		t.Fatalf("normal grant: %s", res.Status)
	}
	res, _ := e.RequestReservation(ctx, req("high", fleetv1.ActionPriorityHigh))
	if res.Status != PreemptedGranted {
		t.Fatalf("high: status=%s, want PreemptedGranted", res.Status)
	}
	if len(res.PreemptedActionIDs) != 1 || res.PreemptedActionIDs[0] != "normal" {
		t.Fatalf("preempted = %v, want [normal]", res.PreemptedActionIDs)
	}
}

// A High action never preempts another High (FIFO within the preemptor bands) nor a
// Critical reservation — only Normal/Low are preemptible victims.
func TestRequestReservation_HighDoesNotPreemptHighOrCritical(t *testing.T) {
	ctx := context.Background()
	for _, victim := range []fleetv1.ActionPriority{fleetv1.ActionPriorityHigh, fleetv1.ActionPriorityCritical} {
		e, _ := newEngine(t, fzone("z", 1))
		if res, _ := e.RequestReservation(ctx, req("holder", victim)); res.Status != Granted {
			t.Fatalf("%s holder grant: %s", victim, res.Status)
		}
		res, _ := e.RequestReservation(ctx, req("high", fleetv1.ActionPriorityHigh))
		if res.Status != Denied {
			t.Fatalf("High preempted a %s reservation (%s); must be Denied (Normal/Low only)", victim, res.Status)
		}
	}
}

func TestOnRobotEnteredZone_ReservedToOccupied(t *testing.T) {
	e, c := newEngine(t, fzone("z", 2))
	ctx := context.Background()
	_, _ = e.RequestReservation(ctx, req("t1", fleetv1.ActionPriorityNormal))
	if err := e.OnRobotEnteredZone(ctx, ns, "z", "robot-t1"); err != nil {
		t.Fatalf("entered: %v", err)
	}
	z := &fleetv1.FleetZone{}
	_ = c.Get(ctx, types.NamespacedName{Name: "z", Namespace: ns}, z)
	if len(z.Status.Reservations) != 1 || z.Status.Reservations[0].State != fleetv1.ReservationOccupied {
		t.Fatalf("reservation not Occupied: %+v", z.Status.Reservations)
	}
	if z.Status.CurrentConcurrentRobots != 1 {
		t.Fatalf("currentConcurrentRobots = %d, want 1", z.Status.CurrentConcurrentRobots)
	}
}

func TestSharedResource_QueueOrderingByPriority(t *testing.T) {
	lift := fleetv1.SharedResource{Name: "lift", Type: fleetv1.SharedResourceElevator, Capacity: 1, ReservationPolicy: fleetv1.ReservationPriority}
	e, c := newEngine(t, fzone("z", 0, lift))
	ctx := context.Background()

	resReq := func(id string, prio fleetv1.ActionPriority) ReservationRequest {
		return ReservationRequest{ActionID: id, RobotID: "r-" + id, Namespace: ns, Priority: prio,
			Resources: []ResourceRequest{{ResourceName: "lift", ZoneName: "z"}}}
	}
	_, _ = e.RequestReservation(ctx, resReq("holder", fleetv1.ActionPriorityNormal)) // becomes holder
	_, _ = e.RequestReservation(ctx, resReq("low", fleetv1.ActionPriorityLow))
	_, _ = e.RequestReservation(ctx, resReq("high", fleetv1.ActionPriorityHigh))
	_, _ = e.RequestReservation(ctx, resReq("normal", fleetv1.ActionPriorityNormal))

	z := &fleetv1.FleetZone{}
	_ = c.Get(ctx, types.NamespacedName{Name: "z", Namespace: ns}, z)
	q := z.Status.SharedResourceQueues[0]
	if len(q.CurrentHolders) != 1 || q.CurrentHolders[0].ActionID != "holder" {
		t.Fatalf("holders = %+v, want [holder]", q.CurrentHolders)
	}
	order := []string{q.WaitQueue[0].ActionID, q.WaitQueue[1].ActionID, q.WaitQueue[2].ActionID}
	if order[0] != "high" || order[1] != "normal" || order[2] != "low" {
		t.Fatalf("wait-queue order = %v, want [high normal low] (priority band DESC)", order)
	}
}

func TestReleaseReservation_PromotesQueue(t *testing.T) {
	lift := fleetv1.SharedResource{Name: "lift", Type: fleetv1.SharedResourceElevator, Capacity: 1, ReservationPolicy: fleetv1.ReservationFIFO}
	e, c := newEngine(t, fzone("z", 0, lift))
	ctx := context.Background()
	resReq := func(id string) ReservationRequest {
		return ReservationRequest{ActionID: id, RobotID: "r-" + id, Namespace: ns, Priority: fleetv1.ActionPriorityNormal,
			Resources: []ResourceRequest{{ResourceName: "lift", ZoneName: "z"}}}
	}
	_, _ = e.RequestReservation(ctx, resReq("a")) // holder
	_, _ = e.RequestReservation(ctx, resReq("b")) // queued
	_ = e.ReleaseReservation(ctx, ns, "z", "a")

	z := &fleetv1.FleetZone{}
	_ = c.Get(ctx, types.NamespacedName{Name: "z", Namespace: ns}, z)
	q := z.Status.SharedResourceQueues[0]
	if len(q.CurrentHolders) != 1 || q.CurrentHolders[0].ActionID != "b" {
		t.Fatalf("after release, holders = %+v, want [b] (promoted from queue)", q.CurrentHolders)
	}
	if len(q.WaitQueue) != 0 {
		t.Fatalf("wait queue should be empty, got %+v", q.WaitQueue)
	}
}

// Reserve-then-crash durable half: a Reserved entry that is never confirmed
// Occupied expires at its TTL and is excluded from the capacity count, so the
// slot is freed even if the in-process Unreserve never ran (real process crash).
func TestRequestReservation_ExpiredReservedFreesCapacity(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 1))
	base := time.Unix(1_700_000_000, 0)
	cur := base
	e.WithClock(func() time.Time { return cur })
	ctx := context.Background()

	if res, _ := e.RequestReservation(ctx, req("t1", fleetv1.ActionPriorityNormal)); res.Status != Granted {
		t.Fatalf("t1: %s", res.Status)
	}
	// Zone full while t1's Reserved entry is live.
	if res, _ := e.RequestReservation(ctx, req("t2", fleetv1.ActionPriorityNormal)); res.Status != Denied {
		t.Fatalf("t2 while t1 live: %s, want Denied", res.Status)
	}
	// Advance past the reservation TTL — t1's Reserved entry is now stale.
	cur = base.Add(DefaultConfig().ReservationTTL + time.Second)
	if res, _ := e.RequestReservation(ctx, req("t3", fleetv1.ActionPriorityNormal)); res.Status != Granted {
		t.Fatalf("t3 after t1 TTL expiry: %s, want Granted (expired Reserved slot freed)", res.Status)
	}
}

// Review-pass concurrency test: M>N concurrent requests for a capacity-N zone.
// Exactly N are Granted and the rest Denied — no double-grant / over-capacity.
// Run under -race.
func TestRequestReservation_ConcurrentNoDoubleGrant(t *testing.T) {
	const capacity = 3
	const requesters = 24
	e, c := newEngine(t, fzone("z", capacity))

	var granted int64
	var wg sync.WaitGroup
	for i := 0; i < requesters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.RequestReservation(context.Background(), req(fmt.Sprintf("t%d", i), fleetv1.ActionPriorityNormal))
			if err == nil && res.Status == Granted {
				atomic.AddInt64(&granted, 1)
			}
		}(i)
	}
	wg.Wait()

	if granted != capacity {
		t.Fatalf("granted = %d, want exactly %d (no double-grant, no over-capacity)", granted, capacity)
	}
	// The mirrored status must also show exactly capacity reservations.
	z := &fleetv1.FleetZone{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "z", Namespace: ns}, z)
	if len(z.Status.Reservations) != capacity {
		t.Fatalf("status reservations = %d, want %d", len(z.Status.Reservations), capacity)
	}
}
