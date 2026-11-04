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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FleetAdapter.status.supportedActions[].results — what an action EMITS.
//
// SupportedAction declared inputs (`params`) and nothing about outputs, so a
// binding feeding one step's output into a later step's input was checkable at the
// consuming end only. `results` is the producing end.
//
// This is an envtest because the failure it guards is schema presence, not Go
// behaviour: a status field absent from the generated CRD is PRUNED by the API
// server on write and comes back empty, with no error at either end. The fake
// client prunes nothing, so it cannot tell a projected field from a discarded one.
// FleetZone.spec.memberZones sat in exactly that state — specified in RFC-0001,
// absent from the CRD, silently dropped on every apply — which is the precedent
// this test exists to avoid repeating.

func TestEnvtest_SupportedActionResultsRoundTrip(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-adapter", Namespace: ns},
		Spec: fleetv1.FleetAdapterSpec{
			Vendor:   "acme-robotics",
			Endpoint: "acme-adapter.svc:50051",
		},
	}
	if err := envK8s.Create(ctx, fa); err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	fa.Status.SupportedActions = []fleetv1.SupportedAction{{
		ActionType: "Inspect",
		Params: []fleetv1.ActionParam{
			{Name: "target", Kind: "string", Required: true},
		},
		Results: []fleetv1.ActionParam{
			{Name: "label", Kind: "enum", Allowed: []string{"pass", "fail"}, Required: true},
			{Name: "confidence", Kind: "number", Unit: "", Required: false},
		},
	}}
	if err := envK8s.Status().Update(ctx, fa); err != nil {
		t.Fatalf("status update declaring results[] was rejected: %v", err)
	}

	var got fleetv1.FleetAdapter
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "acme-adapter", Namespace: ns}, &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.Status.SupportedActions) != 1 {
		t.Fatalf("supportedActions did not round-trip: %#v", got.Status.SupportedActions)
	}
	res := got.Status.SupportedActions[0].Results
	if len(res) != 2 {
		t.Fatalf("results[] did not round-trip: declared 2, read back %d (%v). A results list "+
			"the API server discards is worse than none: an adapter would declare its outputs, "+
			"the apply would succeed, and the binding checker would still see an action that "+
			"states nothing about what it emits", len(res), res)
	}
	if res[0].Name != "label" || res[0].Kind != "enum" || len(res[0].Allowed) != 2 || !res[0].Required {
		t.Errorf("the enum result lost detail in the round trip: %#v — allowed[] and required are "+
			"what a static binding check reads", res[0])
	}
	// params must be untouched by the addition.
	if len(got.Status.SupportedActions[0].Params) != 1 {
		t.Errorf("params[] changed when results[] was added: %#v", got.Status.SupportedActions[0].Params)
	}
}

// The wire type carries the field too, and field 5 is what the CRD mirrors. This
// guards the proto half against a regenerated stub silently losing the field.
func TestSupportedActionResultsOnTheWireType(t *testing.T) {
	sa := &fav1.SupportedAction{
		ActionType: "Inspect",
		Results: []*fav1.ActionParam{
			{Name: "label", Kind: "enum", Allowed: []string{"pass", "fail"}, Required: true},
		},
	}
	if len(sa.GetResults()) != 1 {
		t.Fatalf("fleet_adapter.v1 SupportedAction.results is not carried by the generated stub")
	}
	if sa.GetResults()[0].GetName() != "label" {
		t.Errorf("results[0].name = %q, want \"label\"", sa.GetResults()[0].GetName())
	}
	// An adapter that declares no results means "makes no statement about outputs",
	// NOT "produces nothing" — every adapter predating field 5 looks like this.
	var empty fav1.SupportedAction
	if empty.GetResults() != nil {
		t.Errorf("an adapter declaring no results must read as empty/unknown, got %#v", empty.GetResults())
	}
}
