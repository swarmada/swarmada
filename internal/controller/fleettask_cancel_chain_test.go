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
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A dependency chain cancelled after its first member was created. The later members
// are never created — each waits on a predecessor that will never start it — so they
// are as finished as the cancelled one. The task ends Cancelled, the operator's intent,
// not Failed (RFC-0001 crds/fleettask.md: a desiredState Cancelled write is the path
// into Cancelled). This is the shape of the shipped sample, three members deep.
func TestFleetTask_CancelledChainReportsCancelled(t *testing.T) {
	ft := fleetTask("ft-chain", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("approach", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("inspect", []string{"approach"}, fleetv1.ActionPhaseSucceeded),
		ftAction("return", []string{"inspect"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft, childAction("ft-chain", "approach", fleetv1.ActionPhaseCancelled))

	reconcileFT(t, r, "ft-chain")

	if got := getFT(t, c, "ft-chain").Status.Phase; got != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("phase = %q, want Cancelled: a member behind a never-created predecessor was counted "+
			"as still running, so the cancellation was reported as a failure", got)
	}
}

// The same unreachable tail under ContinueOthers: once the chain's head has failed,
// nothing behind it can run, so the task is terminal. Counting the tail as running
// left it Running for ever.
func TestFleetTask_ContinueOthersChainWithFailedHeadTerminates(t *testing.T) {
	ft := fleetTask("ft-co", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyContinueOthers,
		fleetv1.DesiredStateRunning,
		ftAction("approach", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("inspect", []string{"approach"}, fleetv1.ActionPhaseSucceeded),
		ftAction("return", []string{"inspect"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft, childAction("ft-co", "approach", fleetv1.ActionPhaseFailed))

	reconcileFT(t, r, "ft-co")

	if got := getFT(t, c, "ft-co").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed: the tail of a chain whose head failed can never run", got)
	}
}

// The same cancel landing one member later: the head has Succeeded, the second member
// was created and is Cancelled, the third was never created. Still Cancelled.
func TestFleetTask_CancelledChainAfterItsHeadSucceededReportsCancelled(t *testing.T) {
	ft := fleetTask("ft-chain2", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("approach", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("inspect", []string{"approach"}, fleetv1.ActionPhaseSucceeded),
		ftAction("return", []string{"inspect"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-chain2", "approach", fleetv1.ActionPhaseSucceeded),
		childAction("ft-chain2", "inspect", fleetv1.ActionPhaseCancelled))

	reconcileFT(t, r, "ft-chain2")

	if got := getFT(t, c, "ft-chain2").Status.Phase; got != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("phase = %q, want Cancelled", got)
	}
}

// A cancellation in flight: one member has reached Cancelled, another is still
// winding down. The policy is already unmeetable, but that is the cancellation taking
// effect, not a failure: the task must not be failed (FailFast) on the way to
// Cancelled. It ends Cancelled once every member is terminal.
func TestFleetTask_CancellationInFlightIsNotFailed(t *testing.T) {
	ft := fleetTask("ft-inflight", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-inflight", "a", fleetv1.ActionPhaseCancelled),
		childAction("ft-inflight", "b", fleetv1.ActionPhaseInProgress))

	reconcileFT(t, r, "ft-inflight")
	if got := getFT(t, c, "ft-inflight").Status.Phase; got == fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q while the cancellation is still taking effect; an operator cancel is not a failure", got)
	}

	b := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: childName("ft-inflight", "b")}, b); err != nil {
		t.Fatal(err)
	}
	b.Status.Phase = fleetv1.ActionPhaseCancelled
	if err := c.Status().Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	reconcileFT(t, r, "ft-inflight")
	if got := getFT(t, c, "ft-inflight").Status.Phase; got != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("phase = %q once every member is terminal, want Cancelled", got)
	}
}
