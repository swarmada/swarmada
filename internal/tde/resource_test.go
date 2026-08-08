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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func liftResource() fleetv1.SharedResource {
	return fleetv1.SharedResource{Name: "lift", Type: fleetv1.SharedResourceElevator, Capacity: 1, ReservationPolicy: fleetv1.ReservationFIFO}
}

// The first robot to reserve a free resource is granted; a second is queued behind
// it; releasing the holder promotes the queued robot (§5.4.5).
func TestReserveResource_GrantQueuePromote(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 0, liftResource()))
	ctx := context.Background()

	if out, _ := e.ReserveResource(ctx, ns, "lift", "r1"); out.State != ResourceGranted {
		t.Fatalf("r1 reserve = %s, want Granted", out.State)
	}
	out, _ := e.ReserveResource(ctx, ns, "lift", "r2")
	if out.State != ResourceQueued || out.QueuePosition != 1 {
		t.Fatalf("r2 reserve = %s pos=%d, want Queued pos=1", out.State, out.QueuePosition)
	}

	// Idempotent: r2 asking again reports its existing queued position, not a dup.
	if again, _ := e.ReserveResource(ctx, ns, "lift", "r2"); again.State != ResourceQueued || again.QueuePosition != 1 {
		t.Fatalf("r2 re-reserve = %s pos=%d, want Queued pos=1 (idempotent)", again.State, again.QueuePosition)
	}

	// Release the holder → the queued robot is promoted to holder.
	rel, _ := e.ReleaseResource(ctx, ns, "lift", "r1")
	if !rel.Released {
		t.Fatal("r1 release should report Released")
	}
	if rel.PromotedRobotID != "r2" {
		t.Fatalf("PromotedRobotID = %q, want r2 (queued waiter promoted on release)", rel.PromotedRobotID)
	}
	if promoted, _ := e.ReserveResource(ctx, ns, "lift", "r2"); promoted.State != ResourceGranted {
		t.Fatalf("r2 after r1 release = %s, want Granted (promoted)", promoted.State)
	}
}

// FAIL-CLOSED: a resource no zone declares is Denied, never granted.
func TestReserveResource_UnknownResourceDeniedFailClosed(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 0, liftResource()))
	out, err := e.ReserveResource(context.Background(), ns, "teleporter", "r1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.State != ResourceDenied {
		t.Fatalf("unknown resource = %s, want Denied (fail closed)", out.State)
	}
}

// FAIL-CLOSED: an engine that has not recovered reservation state denies a resource
// reservation (no grant against an unrebuilt queue).
func TestReserveResource_FailsClosedUntilRecovered(t *testing.T) {
	// Build an UNrecovered engine directly (newEngine marks recovered).
	c := recoveryClient(t, fzone("z", 0, liftResource()))
	e := New(c, DefaultConfig())
	if out, _ := e.ReserveResource(context.Background(), ns, "lift", "r1"); out.State != ResourceDenied {
		t.Fatalf("pre-recovery reserve = %s, want Denied (fail closed)", out.State)
	}
	e.SetRecovered(true)
	if out, _ := e.ReserveResource(context.Background(), ns, "lift", "r1"); out.State != ResourceGranted {
		t.Fatalf("post-recovery reserve = %s, want Granted", out.State)
	}
}

// Releasing a resource the robot never held reports Released=false (not an error).
func TestReleaseResource_NotHeld(t *testing.T) {
	e, _ := newEngine(t, fzone("z", 0, liftResource()))
	if rel, _ := e.ReleaseResource(context.Background(), ns, "lift", "ghost"); rel.Released {
		t.Fatal("releasing an unheld resource must report Released=false")
	}
}
