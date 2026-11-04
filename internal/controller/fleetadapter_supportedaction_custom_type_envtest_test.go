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
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FleetAdapter.status.supportedActions[].customType — one catalog entry per Custom verb.
//
// An envtest for the same reason as the results round trip beside it: a status field
// absent from the generated CRD is pruned on write with no error at either end, and the
// fake client prunes nothing. It also pins the second half of the projection rule: the
// CRD bounds the verb, so an over-long one would refuse the whole write, which is why
// mapSupportedActions drops that entry instead of projecting it.
func TestEnvtest_SupportedActionCustomTypeRoundTrip(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "ros2-adapter", Namespace: ns},
		Spec:       fleetv1.FleetAdapterSpec{Vendor: "acme-robotics", Endpoint: "ros2-adapter.svc:50051"},
	}
	if err := envK8s.Create(ctx, fa); err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	// The projection the ingestor writes, from a catalog with a verb no action can name.
	fa.Status.SupportedActions = mapSupportedActions([]*fav1.SupportedAction{
		{ActionType: "Navigate"},
		{ActionType: "Custom", CustomType: "explore-floor"},
		{ActionType: "Custom", CustomType: "manual-drive"},
		{ActionType: "Custom", CustomType: strings.Repeat("v", 64)},
	})
	if err := envK8s.Status().Update(ctx, fa); err != nil {
		t.Fatalf("status update with two keyed Custom entries was rejected: %v", err)
	}
	var got fleetv1.FleetAdapter
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "ros2-adapter", Namespace: ns}, &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	var verbs []string
	for _, sa := range got.Status.SupportedActions {
		verbs = append(verbs, sa.ActionType+"/"+sa.CustomType)
	}
	if strings.Join(verbs, ",") != "Navigate/,Custom/explore-floor,Custom/manual-drive" {
		t.Fatalf("supportedActions read back as %v, want Navigate, then the two keyed Custom "+
			"entries with their verbs, and the over-long one dropped", verbs)
	}

	// The bound the drop protects against: written directly, the over-long verb costs the
	// write — and every entry in it.
	got.Status.SupportedActions = append(got.Status.SupportedActions,
		fleetv1.SupportedAction{ActionType: "Custom", CustomType: strings.Repeat("v", 64)})
	if err := envK8s.Status().Update(ctx, &got); err == nil {
		t.Fatal("the CRD accepted a 64-character customType; spec.customType's bound is 63, and " +
			"the status mirror must carry the same bound")
	}
}
