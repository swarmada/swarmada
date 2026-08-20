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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// crds/fleettask.md states that the conditional `spec.quorum` requirement "is
// enforced with CEL XValidation at admission". That claim had never been
// exercised: every FleetTask test used the fake client, which performs no
// admission at all and accepts objects the API server rejects.
//
// This is the first test to put the rule in front of a real API server. It
// matters beyond the one field, because the failure it guards against is
// silent: a CEL rule with a typo, a wrong CEL path, or a rule that CRD
// regeneration dropped all behave identically to a correct one under the fake
// client — the object is accepted either way, and nothing ever says so.
//
// ITEM-0050 is why this particular rule earns a test: aggregate() treats a nil
// spec.quorum under completionPolicy: Quorum as quorum 0, so a task that slipped
// past admission reports Succeeded having done nothing. Admission is the only
// thing standing between that bug and a user.
func TestEnvtest_FleetTaskQuorumCELRequired(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	mk := func(name string, cp fleetv1.CompletionPolicy, quorum *int32) *fleetv1.FleetTask {
		return &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: fleetv1.FleetTaskSpec{
				CompletionPolicy: cp,
				Quorum:           quorum,
				FailurePolicy:    fleetv1.FailurePolicyFailFast,
				DesiredState:     fleetv1.DesiredStateRunning,
				Actions: []fleetv1.FleetTaskAction{{
					Name:   "a",
					Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
				}},
			},
		}
	}
	two := int32(2)

	// The rule under test: Quorum without spec.quorum must be REJECTED.
	err := envK8s.Create(ctx, mk("ft-quorum-missing", fleetv1.CompletionPolicyQuorum, nil))
	if err == nil {
		t.Fatal("the API server accepted completionPolicy: Quorum with no spec.quorum — " +
			"the CEL XValidation rule crds/fleettask.md promises is not in force. " +
			"aggregate() will read the nil quorum as 0 and report Succeeded having done nothing (ITEM-0050)")
	}
	if !strings.Contains(err.Error(), "quorum is required") {
		t.Errorf("rejected, but not by the expected rule — message was: %v", err)
	}

	// The three cases that must still be ACCEPTED, so the rule is not merely
	// rejecting everything. A validation test that proves only a rejection
	// cannot distinguish a correct rule from an overly broad one.
	for _, ok := range []struct {
		name   string
		policy fleetv1.CompletionPolicy
		quorum *int32
	}{
		{"ft-quorum-present", fleetv1.CompletionPolicyQuorum, &two},
		{"ft-all-no-quorum", fleetv1.CompletionPolicyAll, nil},
		{"ft-any-no-quorum", fleetv1.CompletionPolicyAny, nil},
	} {
		if err := envK8s.Create(ctx, mk(ok.name, ok.policy, ok.quorum)); err != nil {
			t.Errorf("%s: the rule rejected a valid object (%s, quorum set: %t): %v",
				ok.name, ok.policy, ok.quorum != nil, err)
		}
	}
}
