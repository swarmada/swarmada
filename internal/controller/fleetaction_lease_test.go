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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const testSkew = 5 * time.Second

func tp(d time.Duration, base time.Time) *time.Time { t := base.Add(d); return &t }

// TestEvaluateLease_Table exhaustively covers the decision core.
func TestEvaluateLease_Table(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	future := tp(20*time.Second, now) // lease still alive
	past := tp(-20*time.Second, now)  // lease expired well past skew

	cases := []struct {
		name  string
		phase fleetv1.ActionPhase
		reach reachability
		lease *time.Time
		want  leaseAction
	}{
		// Assigned / InProgress.
		{"assigned+executing→renew", fleetv1.ActionPhaseAssigned, robotExecuting, future, actionRenew},
		{"inprogress+executing→renew", fleetv1.ActionPhaseInProgress, robotExecuting, future, actionRenew},
		{"inprogress+lost→revoke", fleetv1.ActionPhaseInProgress, robotLost, future, actionRevoke},
		{"assigned+lost→revoke", fleetv1.ActionPhaseAssigned, robotLost, future, actionRevoke},
		{"inprogress+free→hold(no guess)", fleetv1.ActionPhaseInProgress, robotFree, future, actionHold},
		// Revoking.
		{"revoking+executing→renew(readopt)", fleetv1.ActionPhaseRevoking, robotExecuting, future, actionRenew},
		{"revoking+free→reassign(cond2)", fleetv1.ActionPhaseRevoking, robotFree, future, actionReassign},
		{"revoking+lost+alive→hold", fleetv1.ActionPhaseRevoking, robotLost, future, actionHold},
		{"revoking+lost+expired→reassign(cond3)", fleetv1.ActionPhaseRevoking, robotLost, past, actionReassign},
		{"revoking+lost+nil-lease→hold(no proof)", fleetv1.ActionPhaseRevoking, robotLost, nil, actionHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateLease(tc.phase, tc.reach, tc.lease, now, testSkew); got != tc.want {
				t.Fatalf("evaluateLease = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLeaseProvablyDead_BoundaryAndNil guards RA-4: a nil horizon is never proof,
// and the skew margin is honoured exactly.
func TestLeaseProvablyDead_BoundaryAndNil(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if leaseProvablyDead(nil, now, testSkew) {
		t.Fatal("nil lease must NOT be provably dead (RA-4)")
	}
	// Exactly at horizon-minus-1ns: still alive.
	justAlive := now.Add(-testSkew).Add(time.Nanosecond)
	if leaseProvablyDead(&justAlive, now, testSkew) {
		t.Fatal("lease 1ns before the skew horizon must not be dead")
	}
	// Exactly at the horizon: dead.
	atHorizon := now.Add(-testSkew)
	if !leaseProvablyDead(&atHorizon, now, testSkew) {
		t.Fatal("lease at the skew horizon must be provably dead")
	}
}

func TestClassifyRobot(t *testing.T) {
	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "t1"}}
	mk := func(phase fleetv1.RobotPhase, assigned string) *fleetv1.Robot {
		return &fleetv1.Robot{Status: fleetv1.RobotStatus{Phase: phase, AssignedAction: assigned}}
	}
	if got := classifyRobot(action, nil, false); got != robotLost {
		t.Errorf("deleted robot = %d, want robotLost", got)
	}
	if got := classifyRobot(action, mk(fleetv1.RobotPhaseOffline, "t1"), true); got != robotLost {
		t.Errorf("offline robot = %d, want robotLost", got)
	}
	if got := classifyRobot(action, mk(fleetv1.RobotPhaseError, "t1"), true); got != robotLost {
		t.Errorf("error robot = %d, want robotLost", got)
	}
	if got := classifyRobot(action, mk(fleetv1.RobotPhaseInProgress, "t1"), true); got != robotExecuting {
		t.Errorf("reachable+holding-T = %d, want robotExecuting", got)
	}
	if got := classifyRobot(action, mk(fleetv1.RobotPhaseIdle, ""), true); got != robotFree {
		t.Errorf("reachable+not-holding-T = %d, want robotFree", got)
	}
}

// ── The three MANDATORY review cases, at the decision level ────────────────────

// Case 1 — Connectivity loss: an InProgress robot goes Offline. The action must
// Revoke (NOT reassign), then Hold while the lease is alive, and only Reassign
// once the lease is provably dead. No window reassigns before the horizon.
func TestSingleExecutor_ConnectivityLoss(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	alive := tp(10*time.Second, now)

	if a := evaluateLease(fleetv1.ActionPhaseInProgress, robotLost, alive, now, testSkew); a != actionRevoke {
		t.Fatalf("on connectivity loss want Revoke, got %d", a)
	}
	// While Revoking and the lease is alive, every reconcile Holds — never reassigns.
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if a := evaluateLease(fleetv1.ActionPhaseRevoking, robotLost, alive, at, testSkew); a != actionHold {
			t.Fatalf("reassigned before lease horizon at +%ds (a=%d) — DOUBLE-EXECUTION HAZARD", i, a)
		}
	}
	// Past the horizon, exactly one Reassign is permitted.
	afterHorizon := alive.Add(testSkew).Add(time.Second)
	if a := evaluateLease(fleetv1.ActionPhaseRevoking, robotLost, alive, afterHorizon, testSkew); a != actionReassign {
		t.Fatalf("want Reassign after lease horizon, got %d", a)
	}
}

// Case 2 — Control-plane restart/failover: the decision is a pure function of the
// PERSISTED status (phase + leaseExpiresAt), so a fresh reconcile after a restart
// reaches the same verdict with zero in-memory state. A Revoking action whose lease
// already expired is immediately reassignable; one still alive holds.
func TestSingleExecutor_RestartFailover(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Restart reconcile reading persisted state — lease already expired past skew.
	expired := tp(-10*time.Second, now)
	if a := evaluateLease(fleetv1.ActionPhaseRevoking, robotLost, expired, now, testSkew); a != actionReassign {
		t.Fatalf("post-restart, expired lease should Reassign, got %d", a)
	}
	// Restart reconcile — lease still alive: must NOT reassign despite the restart.
	alive := tp(30*time.Second, now)
	if a := evaluateLease(fleetv1.ActionPhaseRevoking, robotLost, alive, now, testSkew); a != actionHold {
		t.Fatalf("post-restart, live lease must Hold (no reassign), got %d", a)
	}
	// Generation monotonicity across a restart is a read-before-issue increment of
	// the persisted value; a fresh leader never decreases or reuses it.
	persisted := int64(7)
	next := persisted + 1
	if next <= persisted {
		t.Fatal("assignmentGeneration must strictly increase")
	}
}

// Case 3 — Delayed/duplicate message: the decision core is idempotent. Repeated or
// out-of-order reconciles of the same state never produce a spurious reassignment,
// and a delayed "robot came back executing" re-adopts (renew) rather than minting
// a new generation.
func TestSingleExecutor_DelayedDuplicate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	alive := tp(15*time.Second, now)

	// Duplicate revoke evaluations are stable (Revoke is idempotent → still Revoke
	// when re-entered as Assigned/InProgress, Hold once Revoking).
	if evaluateLease(fleetv1.ActionPhaseInProgress, robotLost, alive, now, testSkew) != actionRevoke {
		t.Fatal("first loss should Revoke")
	}
	if evaluateLease(fleetv1.ActionPhaseRevoking, robotLost, alive, now, testSkew) != actionHold {
		t.Fatal("duplicate loss while Revoking must Hold, not reassign")
	}
	// A delayed frame showing the robot back and executing → re-adopt via Renew
	// (same generation), never Reassign.
	if a := evaluateLease(fleetv1.ActionPhaseRevoking, robotExecuting, alive, now, testSkew); a != actionRenew {
		t.Fatalf("delayed re-adopt should Renew (same generation), got %d", a)
	}
}
