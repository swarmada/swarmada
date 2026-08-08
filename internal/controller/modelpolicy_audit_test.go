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

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// captureRecorder records audit entries in memory for assertion.
type captureRecorder struct{ entries []audit.Entry }

func (c *captureRecorder) Record(e audit.Entry) (audit.Entry, error) {
	c.entries = append(c.entries, e)
	return e, nil
}

// Auto-creating a ModelRollout on a quality-gate pass seals a MODEL_ROLLOUT_CREATED
// entry into the §9.5.4 audit chain, with the controller as the service-account
// actor and the created rollout as the resource.
func TestModelPolicy_RolloutCreationIsAudited(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/item-recognition:4.1.0", ModelChecksum: testChecksum, Metrics: passingMetrics()}
	r, _ := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig))
	rec := &captureRecorder{}
	r.Audit = rec

	reconcilePolicy(t, r)

	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1 MODEL_ROLLOUT_CREATED", len(rec.entries))
	}
	e := rec.entries[0]
	if e.EventType != audit.EventModelRolloutCreated {
		t.Errorf("event type = %q, want %q", e.EventType, audit.EventModelRolloutCreated)
	}
	if e.Resource.Kind != "ModelRollout" || e.Resource.Name != "item-recognition-policy-4-1-0" {
		t.Errorf("resource = %+v, want ModelRollout item-recognition-policy-4-1-0", e.Resource)
	}
	if e.Actor.Type != audit.ActorServiceAccount {
		t.Errorf("actor type = %q, want service-account (the controller is the creator)", e.Actor.Type)
	}
	if e.Outcome != audit.OutcomeAllowed {
		t.Errorf("outcome = %q, want Allowed", e.Outcome)
	}
	if e.Detail["version"] != "4.1.0" || e.Detail["model"] != "item-recognition" {
		t.Errorf("detail = %+v", e.Detail)
	}
}

// A nil Audit recorder must not panic (audit is best-effort; the rollout still
// creates). The other ModelPolicy tests run with a nil recorder and pass, but make
// the guarantee explicit here.
func TestModelPolicy_NilAuditRecorderIsSafe(t *testing.T) {
	trig := modelTriggerPayload{ModelVersion: "4.1.0", ModelURI: "oci://reg/item-recognition:4.1.0", ModelChecksum: testChecksum, Metrics: passingMetrics()}
	r, c := newPolicyReconciler(t, policyWithGate(fleetv1.AutoDeployQualityGatePass, strictGate(), trig))
	// r.Audit is nil.
	reconcilePolicy(t, r)

	ro := &fleetv1.ModelRollout{}
	key := types.NamespacedName{Name: "item-recognition-policy-4-1-0", Namespace: rolloutNS}
	if err := c.Get(context.Background(), key, ro); err != nil {
		t.Fatalf("rollout should still be created with a nil audit recorder: %v", err)
	}
}
