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
	"encoding/json"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func failingMetrics() map[string]float64 {
	// pick_success_rate 0.5 < strictGate's 0.95 → a guaranteed gate rejection.
	return map[string]float64{"pick_success_rate": 0.5, "failure_rate": 0.5, "eval_episodes": 800}
}

// rearmTrigger (re)writes the trigger annotation so the next reconcile evaluates.
func rearmTrigger(t *testing.T, r *ModelPolicyReconciler, payload modelTriggerPayload) {
	t.Helper()
	p := getPolicy(t, r.Client)
	raw, _ := json.Marshal(payload)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[triggerAnnotation] = string(raw)
	if err := r.Update(context.Background(), p); err != nil {
		t.Fatalf("rearm trigger: %v", err)
	}
}

// After consecutiveRejectionLimit rejections the policy suspends: FailedRepeatedly
// is set and a further trigger is silently dropped (not evaluated).
func TestModelPolicy_SuspendsAfterConsecutiveRejections(t *testing.T) {
	failing := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://r/m:4.1.0", Metrics: failingMetrics()}
	pol := policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), failing)
	pol.Spec.ConsecutiveRejectionLimit = 3
	r, c := newPolicyReconciler(t, pol)

	for i := 1; i <= 3; i++ {
		rearmTrigger(t, r, failing)
		reconcilePolicy(t, r)
	}

	p := getPolicy(t, c)
	if p.Status.ConsecutiveRejections != 3 {
		t.Fatalf("consecutiveRejections = %d, want 3", p.Status.ConsecutiveRejections)
	}
	if !apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionFailedRepeatedly) {
		t.Fatal("FailedRepeatedly condition not set after reaching the limit")
	}
	if p.Status.RejectionCount != 3 {
		t.Fatalf("rejectionCount = %d, want 3", p.Status.RejectionCount)
	}

	// A further trigger while suspended is dropped without evaluation.
	rearmTrigger(t, r, failing)
	reconcilePolicy(t, r)
	p = getPolicy(t, c)
	if p.Status.RejectionCount != 3 || p.Status.ConsecutiveRejections != 3 {
		t.Fatalf("suspended policy still evaluated: rejections=%d consecutive=%d", p.Status.RejectionCount, p.Status.ConsecutiveRejections)
	}
	if _, pending := p.Annotations[triggerAnnotation]; pending {
		t.Error("suspended policy did not drop the pending trigger")
	}
}

// A Deploy decision resets the consecutive-rejection streak.
func TestModelPolicy_DeployResetsConsecutiveRejections(t *testing.T) {
	failing := modelTriggerPayload{ModelVersion: "4.0.0", ModelURI: "oci://r/m:4.0.0", Metrics: failingMetrics()}
	pol := policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), failing)
	pol.Spec.ConsecutiveRejectionLimit = 5
	r, c := newPolicyReconciler(t, pol)

	rearmTrigger(t, r, failing)
	reconcilePolicy(t, r)
	rearmTrigger(t, r, failing)
	reconcilePolicy(t, r)
	if getPolicy(t, c).Status.ConsecutiveRejections != 2 {
		t.Fatalf("expected 2 consecutive rejections before the deploy")
	}

	// A passing trigger deploys and resets the streak.
	passing := modelTriggerPayload{ModelVersion: "4.2.0", ModelURI: "oci://r/m:4.2.0", ModelChecksum: testChecksum, Metrics: passingMetrics()}
	rearmTrigger(t, r, passing)
	reconcilePolicy(t, r)

	p := getPolicy(t, c)
	if p.Status.ConsecutiveRejections != 0 {
		t.Fatalf("consecutiveRejections = %d, want 0 after a Deploy", p.Status.ConsecutiveRejections)
	}
	if apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionFailedRepeatedly) {
		t.Error("FailedRepeatedly should not be set below the limit")
	}
}

// consecutiveRejectionLimit=0 disables suspension entirely.
func TestModelPolicy_ZeroLimitNeverSuspends(t *testing.T) {
	failing := modelTriggerPayload{ModelVersion: "4.0.0", ModelURI: "oci://r/m:4.0.0", Metrics: failingMetrics()}
	pol := policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), failing)
	pol.Spec.ConsecutiveRejectionLimit = 0
	r, c := newPolicyReconciler(t, pol)

	for i := 0; i < 5; i++ {
		rearmTrigger(t, r, failing)
		reconcilePolicy(t, r)
	}

	p := getPolicy(t, c)
	if p.Status.ConsecutiveRejections != 5 {
		t.Fatalf("consecutiveRejections = %d, want 5 (counter still tracked)", p.Status.ConsecutiveRejections)
	}
	if apimeta.IsStatusConditionTrue(p.Status.Conditions, conditionFailedRepeatedly) {
		t.Fatal("limit=0 must never suspend")
	}
}
