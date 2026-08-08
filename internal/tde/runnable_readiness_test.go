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
	"time"

	"github.com/go-logr/logr"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// startRunnable runs r.Start until the engine reports recovered, then returns a
// cancel func the caller defers to stop it.
func startRunnable(t *testing.T, r *RecoveryRunnable) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	waitFor(t, r.Engine.Recovered, 2*time.Second, "engine recovered")
	return func() {
		cancel()
		<-done
	}
}

// When the Zone Controller is ready, recovery uses RecoverValidate: an Occupied
// reservation whose robot is still in the zone is kept and the Reserved one is
// released (§9.4.7).
func TestRecoveryRunnable_ZoneControllerReady_Validates(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t-occ", "r-occ", fleetv1.ReservationOccupied),
			reservation("t-res", "r-res", fleetv1.ReservationReserved),
		),
		robotInZone("r-occ", "z"),
	)
	e := New(c, DefaultConfig())
	r := &RecoveryRunnable{
		Engine: e, Client: c, Mode: RecoverValidate,
		Ready: func() bool { return true }, ReadyTimeout: time.Second, Fallback: RecoverReleaseAll,
		Log: logr.Discard(),
	}
	defer startRunnable(t, r)()

	rs := zoneReservations(t, c, "z")
	if len(rs) != 1 || rs[0].ActionID != "t-occ" {
		t.Fatalf("reservations = %+v, want just [t-occ] (RecoverValidate ran when ready)", rs)
	}
}

// FAIL-SAFE: when the Zone Controller does NOT become ready within the timeout,
// recovery falls back to the conservative action (ReleaseAll) — even an Occupied
// whose robot is present is released, because currentZone cannot be trusted yet
// (§9.4.7). Releasing (never over-retaining) is the safe direction.
func TestRecoveryRunnable_ZoneControllerNotReady_ConservativeFallback(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t-occ", "r-occ", fleetv1.ReservationOccupied),
		),
		robotInZone("r-occ", "z"),
	)
	e := New(c, DefaultConfig())
	r := &RecoveryRunnable{
		Engine: e, Client: c, Mode: RecoverValidate,
		Ready:             func() bool { return false }, // never ready
		ReadyTimeout:      20 * time.Millisecond,
		ReadyPollInterval: 5 * time.Millisecond,
		Fallback:          RecoverReleaseAll,
		Log:               logr.Discard(),
	}
	defer startRunnable(t, r)()

	if rs := zoneReservations(t, c, "z"); len(rs) != 0 {
		t.Fatalf("reservations = %+v, want [] (conservative ReleaseAll ran on not-ready)", rs)
	}
}

// An unset Fallback defaults to ReleaseAll (§9.4.7).
func TestRecoveryRunnable_DefaultFallbackIsReleaseAll(t *testing.T) {
	c := recoveryClient(t,
		zoneWithReservations("z", 3,
			reservation("t-occ", "r-occ", fleetv1.ReservationOccupied),
		),
		robotInZone("r-occ", "z"),
	)
	e := New(c, DefaultConfig())
	r := &RecoveryRunnable{
		Engine: e, Client: c, Mode: RecoverValidate,
		Ready:             func() bool { return false },
		ReadyTimeout:      20 * time.Millisecond,
		ReadyPollInterval: 5 * time.Millisecond,
		// Fallback deliberately unset → defaults to ReleaseAll.
		Log: logr.Discard(),
	}
	defer startRunnable(t, r)()

	if rs := zoneReservations(t, c, "z"); len(rs) != 0 {
		t.Fatalf("reservations = %+v, want [] (unset Fallback defaults to ReleaseAll)", rs)
	}
}
