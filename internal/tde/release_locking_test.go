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
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Lock discipline around ReleaseReservation (Round-4 D-TDE-8).
//
// ReleaseReservation used to lock ONE zone. It now locks EVERY zone in the namespace,
// because a shared resource lives in the state of the zone that DECLARES it, which is
// routinely not the zone the action reserved in — so releasing only the action's own zone
// left holds on an ancestor-declared corridor or lift permanently held, waiters never
// promoted.
//
// Widening a lock set is where deadlocks come from, and zoneState.mu is a plain
// (non-reentrant) sync.Mutex: any caller already holding ANY zone's lock would now
// self-deadlock, and two callers taking different subsets in different orders would
// deadlock against each other. lockZones sorts its keys, which answers the second. These
// tests cover the first, and pin both against regression.
//
// They are written with an explicit timeout rather than left to the package test timeout:
// a deadlock reported as "panic: test timed out after 10m" in another test's stack is
// dramatically harder to attribute than a named failure here.

// mustFinish runs fn and fails if it has not returned within d — the shape a deadlock takes.
func mustFinish(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not complete within %s — lock discipline regression "+
			"(ReleaseReservation locks every zone in the namespace; a caller already "+
			"holding a zone lock self-deadlocks on a non-reentrant mutex)", what, d)
	}
}

// The ONE internal caller of ReleaseReservation. OnActionPhaseChanged locks a zone in its
// Revoking arm; the terminal arm must reach ReleaseReservation with NO lock held. Go's
// switch does not fall through, so it does today — this pins that a future edit hoisting
// the lockZones call above the switch would be caught here rather than in production.
func TestReleaseReservation_TerminalPhaseChangeDoesNotDeadlock(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 4), fzone("z-parent", 4, liftResource()))
	ctx := context.Background()

	if res, err := e.RequestReservation(ctx, req("t1", fleetv1.ActionPriorityNormal)); err != nil || res.Status != Granted {
		t.Fatalf("setup reserve: status=%s err=%v", res.Status, err)
	}
	for _, phase := range []fleetv1.ActionPhase{
		fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseFailed, fleetv1.ActionPhaseCancelled,
	} {
		mustFinish(t, 5*time.Second, fmt.Sprintf("OnActionPhaseChanged(%s)", phase), func() {
			if err := e.OnActionPhaseChanged(ctx, ns, "z", "t1", phase); err != nil {
				t.Errorf("OnActionPhaseChanged(%s): %v", phase, err)
			}
		})
	}
	// The Revoking arm takes its own lock; it must still be reachable afterwards, which
	// proves the terminal arm released everything it took.
	mustFinish(t, 5*time.Second, "OnActionPhaseChanged(Revoking)", func() {
		if err := e.OnActionPhaseChanged(ctx, ns, "z", "t1", fleetv1.ActionPhaseRevoking); err != nil {
			t.Errorf("OnActionPhaseChanged(Revoking): %v", err)
		}
	})
}

// The cross-caller half: many goroutines taking DIFFERENT zone subsets concurrently.
// RequestReservation locks {target, declaring}; ReserveResource locks {declaring};
// ReleaseReservation locks the whole namespace. Without the sorted ordering in lockZones
// these interleave into a classic ABBA deadlock. Run under -race in CI.
func TestReleaseReservation_ConcurrentMixedLockSetsDoNotDeadlock(t *testing.T) {
	e, _ := newEngine(t,
		fzone("z-a", 0, liftResource()),
		fzone("z-b", 0, fleetv1.SharedResource{
			Name: "corridor", Type: fleetv1.SharedResourceCorridor,
			Capacity: 1, ReservationPolicy: fleetv1.ReservationFIFO,
		}),
		fzone("z-c", 0),
	)
	ctx := context.Background()
	zones := []string{"z-a", "z-b", "z-c"}
	resources := []string{"lift", "corridor"}

	mustFinish(t, 30*time.Second, "concurrent reserve/release across zones", func() {
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("t%d", i)
				zone := zones[i%len(zones)]
				r := req(id, fleetv1.ActionPriorityNormal)
				r.TargetZone = zone
				for pass := 0; pass < 8; pass++ {
					_, _ = e.RequestReservation(ctx, r)
					_, _ = e.ReserveResource(ctx, ns, resources[i%len(resources)], id)
					// The wide lock set, interleaved with the narrow ones above.
					_ = e.ReleaseReservation(ctx, ns, zone, id)
				}
			}(i)
		}
		wg.Wait()
	})
}

// The behaviour the wider lock set exists to deliver (D-TDE-8): an action reserved in one
// zone releases a hold on a resource DECLARED IN ANOTHER, and the waiter is promoted.
// Without this, the release is deadlock-free but pointless.
func TestReleaseReservation_FreesHoldOnAResourceDeclaredInAnotherZone(t *testing.T) {
	// The lift is declared by z-parent; the action reserves capacity in z-child.
	e, _ := newEngine(t, fzone("z-child", 4), fzone("z-parent", 4, liftResource()))
	ctx := context.Background()

	r := req("t1", fleetv1.ActionPriorityNormal)
	r.TargetZone = "z-child"
	if res, err := e.RequestReservation(ctx, r); err != nil || res.Status != Granted {
		t.Fatalf("t1 zone reserve: status=%s err=%v", res.Status, err)
	}
	if out, _ := e.ReserveResource(ctx, ns, "lift", "t1"); out.State != ResourceGranted {
		t.Fatalf("t1 lift = %s, want Granted", out.State)
	}
	if out, _ := e.ReserveResource(ctx, ns, "lift", "t2"); out.State != ResourceQueued {
		t.Fatalf("t2 lift = %s, want Queued behind t1", out.State)
	}

	// Release against the action's OWN zone (z-child) — the zone that does not declare
	// the lift. This is precisely the call that used to leave the hold dangling.
	if err := e.ReleaseReservation(ctx, ns, "z-child", "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	if out, _ := e.ReserveResource(ctx, ns, "lift", "t2"); out.State != ResourceGranted {
		t.Fatalf("t2 lift after t1's release = %s, want Granted — the hold on a resource "+
			"declared outside the action's own zone was never released, so the waiter "+
			"is stuck behind a finished action", out.State)
	}
}
