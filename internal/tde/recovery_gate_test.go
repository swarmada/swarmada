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

// FAIL-CLOSED: a freshly built engine has not recovered its reservation state, so
// the grant gate denies with tde_unavailable — a scheduler cannot commit a action
// against empty in-memory state. After Recover the gate opens.
func TestRequestReservation_FailsClosedUntilRecovered(t *testing.T) {
	c := recoveryClient(t, zoneWithReservations("z", 2))
	e := New(c, DefaultConfig())

	if e.Recovered() {
		t.Fatal("a new engine must start NOT recovered (fail closed)")
	}
	res, err := e.RequestReservation(context.Background(), req("t1", fleetv1.ActionPriorityNormal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != Denied || res.DeniedReason != DeniedTDEUnavailable {
		t.Fatalf("pre-recovery = %s/%q, want Denied/%q (fail closed)", res.Status, res.DeniedReason, DeniedTDEUnavailable)
	}

	if rerr := e.Recover(context.Background(), c, RecoverValidate); rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	if !e.Recovered() {
		t.Fatal("Recover must open the grant gate")
	}
	if got, _ := e.RequestReservation(context.Background(), req("t1", fleetv1.ActionPriorityNormal)); got.Status != Granted {
		t.Fatalf("post-recovery = %s, want Granted", got.Status)
	}
}

// The leader-elected RecoveryRunnable recovers on leadership acquisition (opening
// the gate) and re-arms the fail-closed latch on leadership loss, so a promoted
// standby always rebuilds state before serving grants (§9.4.7 + failover).
func TestRecoveryRunnable_RecoversOnLeadershipAndRearmsOnLoss(t *testing.T) {
	c := recoveryClient(t, zoneWithReservations("z", 2))
	e := New(c, DefaultConfig())
	if e.Recovered() {
		t.Fatal("engine must start NOT recovered (fail closed)")
	}

	r := &RecoveryRunnable{Engine: e, Client: c, Mode: RecoverValidate, Log: logr.Discard()}
	if !r.NeedLeaderElection() {
		t.Fatal("recovery must be leader-elected so a failover re-recovers")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Leadership acquired → recovery completes and the gate opens.
	waitFor(t, e.Recovered, 2*time.Second, "engine recovered after leadership acquisition")

	// Leadership lost → the fail-closed latch is re-armed.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runnable returned error: %v", err)
	}
	if e.Recovered() {
		t.Fatal("engine must re-arm fail-closed on loss of leadership (so a re-promotion re-recovers)")
	}
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
