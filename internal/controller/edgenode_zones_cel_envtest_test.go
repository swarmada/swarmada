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

// "A ZoneController MUST list at least one zone; a ComputeNode may list none"
// (RFC-0001 #edgenode). The rule is a CEL rule on EdgeNodeSpec, so it is proven
// here against the real API server: no webhook serves EdgeNode, and a fake client
// performs no admission at all.

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const edgeNodeZonesRule = "a ZoneController must list at least one zone"

func mkEdgeNode(name, ns string, class fleetv1.EdgeNodeClass, zones []string) *fleetv1.EdgeNode {
	return &fleetv1.EdgeNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.EdgeNodeSpec{
			NodeClass: class,
			Identity:  fleetv1.EdgeNodeIdentity{CommonName: "edge-" + name},
			Zones:     zones,
		},
	}
}

func TestEnvtest_EdgeNodeZoneControllerMustListAZone(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	refused := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s was admitted; a ZoneController with no zones holds estop authority over an empty set", what)
			return
		}
		if !strings.Contains(err.Error(), edgeNodeZonesRule) {
			t.Errorf("%s was refused, but not by the zones rule: %v", what, err)
		}
	}

	// Create: absent and empty are the same omission (zones is omitempty).
	refused("a ZoneController with zones absent",
		envK8s.Create(ctx, mkEdgeNode("zc-absent", ns, fleetv1.EdgeNodeClassZoneController, nil)))
	refused("a ZoneController with zones: []",
		envK8s.Create(ctx, mkEdgeNode("zc-empty", ns, fleetv1.EdgeNodeClassZoneController, []string{})))

	// The classes the rule does not cover may list none.
	for _, class := range []fleetv1.EdgeNodeClass{fleetv1.EdgeNodeClassComputeNode, fleetv1.EdgeNodeClassSiteUplink} {
		name := "none-" + strings.ToLower(string(class))
		if err := envK8s.Create(ctx, mkEdgeNode(name, ns, class, nil)); err != nil {
			t.Errorf("a %s with no zones was refused; only a ZoneController must list one: %v", class, err)
		}
	}

	zc := mkEdgeNode("zc-one", ns, fleetv1.EdgeNodeClassZoneController, []string{"warehouse-a"})
	if err := envK8s.Create(ctx, zc); err != nil {
		t.Fatalf("a ZoneController listing one zone was refused: %v", err)
	}

	// Update: the rule holds at every write, not only at birth.
	update := func(name string, mutate func(*fleetv1.EdgeNode)) error {
		cur := &fleetv1.EdgeNode{}
		if err := envK8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cur); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		base := cur.DeepCopy()
		mutate(cur)
		return envK8s.Patch(ctx, cur, client.MergeFrom(base))
	}
	refused("an update emptying a ZoneController's zones",
		update("zc-one", func(n *fleetv1.EdgeNode) { n.Spec.Zones = nil }))
	refused("an update turning a zone-less ComputeNode into a ZoneController",
		update("none-computenode", func(n *fleetv1.EdgeNode) { n.Spec.NodeClass = fleetv1.EdgeNodeClassZoneController }))
	if err := update("zc-one", func(n *fleetv1.EdgeNode) { n.Spec.Zones = []string{"warehouse-a", "dock"} }); err != nil {
		t.Errorf("an update adding a zone to a ZoneController was refused: %v", err)
	}
}
