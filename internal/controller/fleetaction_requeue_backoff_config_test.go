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

// backoffConfig is a namespace SwarmadaConfig carrying only the scheduling backoff.
func backoffConfig(seconds int32) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada", Namespace: actionNS},
		Spec: fleetv1.SwarmadaConfigSpec{
			Scheduling: fleetv1.SwarmadaSchedulingConfig{ActionRequeueBackoffSeconds: seconds},
		},
	}
}

// failedRequeueable is a Failed action that WAS assigned (so a retry applies),
// carrying no spec.retryPolicy — the shape the namespace default is specified to
// fill in.
func failedRequeueable(name string, failedAgo time.Duration) *fleetv1.FleetAction {
	scheduled := metav1.NewTime(time.Now().Add(-failedAgo - time.Minute))
	failed := metav1.NewTime(time.Now().Add(-failedAgo))
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type:      fleetv1.ActionTypeNavigate,
			OnFailure: fleetv1.ActionFailureRequeue,
			// RetryPolicy deliberately nil.
		},
		Status: fleetv1.FleetActionStatus{
			Phase:       fleetv1.ActionPhaseFailed,
			ScheduledAt: &scheduled,
			FailedAt:    &failed,
			RetryCount:  0,
		},
	}
}

// SwarmadaConfig.spec.scheduling.actionRequeueBackoffSeconds is
// declared, defaulted and validated, and was read by nothing: an operator could
// set it, read it back, and observe no change whatsoever.
//
// RFC-0001's override map (#swarmadaconfig, "The full override map") states what
// it governs: it is the namespace default for FleetAction.spec.retryPolicy.
// backoffSeconds — "default backoff before a failed FleetAction is requeued to
// Pending. Applied to FleetAction resources that do not set
// spec.retryPolicy.backoffSeconds explicitly."
func TestFailedAction_HonoursActionRequeueBackoffSeconds(t *testing.T) {
	// Failed 1s ago with a 600s namespace backoff: the retry is still in its window,
	// so the reconcile must hold and requeue near the remaining 599s. Without the
	// config read it falls back to the 30s constant and requeues in ~29s.
	r, _ := newActionReconciler(t, failedRequeueable("t1", time.Second), backoffConfig(600))

	res := reconcileAction(t, r, "t1")
	if res.RequeueAfter < 550*time.Second {
		t.Fatalf("RequeueAfter = %s; want ≈599s from the namespace actionRequeueBackoffSeconds=600. "+
			"The field is still unread and the hardcoded default applied.", res.RequeueAfter)
	}
}

// The per-action value overrides the namespace default — the direction the override
// map specifies, not the other way round.
func TestFailedAction_PerActionRetryPolicyOverridesNamespaceBackoff(t *testing.T) {
	action := failedRequeueable("t1", time.Second)
	action.Spec.RetryPolicy = &fleetv1.ActionRetryPolicy{MaxRetries: 3, BackoffSeconds: 2}

	r, c := newActionReconciler(t, action, backoffConfig(600))

	res := reconcileAction(t, r, "t1")
	// 2s backoff, 1s elapsed → about 1s left, or already elapsed and requeued now.
	if res.RequeueAfter > 5*time.Second {
		t.Fatalf("RequeueAfter = %s; the action's own 2s retryPolicy must win over the namespace's 600s",
			res.RequeueAfter)
	}
	_ = c
}

// No SwarmadaConfig at all: fail safe to the constant that mirrors the CRD default,
// exactly as every other namespace-resolved value in this controller does.
func TestFailedAction_UnreadableConfigFallsBackToDefaultBackoff(t *testing.T) {
	r, _ := newActionReconciler(t, failedRequeueable("t1", time.Second))

	res := reconcileAction(t, r, "t1")
	want := time.Duration(defaultBackoffSeconds) * time.Second
	if res.RequeueAfter > want || res.RequeueAfter < want-5*time.Second {
		t.Fatalf("RequeueAfter = %s; want ≈%s (the CRD default) when no config is readable",
			res.RequeueAfter, want)
	}
}
