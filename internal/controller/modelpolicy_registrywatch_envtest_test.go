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
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A REGISTRY-TRIGGERED ROLLOUT HAS TO PASS ADMISSION.
//
// RegistryWatch polls an OCI registry named by host (RegistryWatchConfig.Registry,
// e.g. "localhost:5000") and, on a new tag whose metrics label passes the quality gate,
// creates a ModelRollout. ModelRollout.spec.modelUri is schema-validated to carry a
// scheme (oci://, s3:// or https://). The fake client runs no schema, so the fake-client
// poll tests pass with any string in that field; this one creates the rollout through a
// real API server, which is the only place the pattern is enforced.
func TestEnvtest_RegistryWatchRolloutPassesAdmission(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	policy := registryWatchPolicy()
	policy.Namespace = ns
	if err := envK8s.Create(ctx, policy); err != nil {
		t.Fatalf("the API server rejected the RegistryWatch ModelPolicy fixture: %v", err)
	}

	r := &ModelPolicyReconciler{Client: envK8s, Scheme: envScheme, Registry: &fakeRegistry{
		tags:           []string{"4.1.0"},
		manifestDigest: testChecksum,
		configDigest:   "sha256:c",
		labels:         map[string]string{"swarmada.metrics": metricsLabelJSON(t, passingMetrics())},
	}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: policy.Name}}
	for pass, what := range []string{"poll (writes the trigger)", "evaluate (creates the rollout)"} {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile pass %d, %s: %v", pass+1, what, err)
		}
	}

	ro := &fleetv1.ModelRollout{}
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "item-recognition-policy-4-1-0"}, ro); err != nil {
		t.Fatalf("no ModelRollout exists after a passing RegistryWatch poll and evaluate: %v\n"+
			"If the controller built spec.modelUri without a scheme, the API server refused the "+
			"create and a registry-triggered rollout can never happen.", err)
	}
	if want := "oci://reg.example:5000/item-recognition:4.1.0"; ro.Spec.ModelURI != want {
		t.Fatalf("spec.modelUri = %q, want %q", ro.Spec.ModelURI, want)
	}
}
