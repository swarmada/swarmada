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

package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newSupportedReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func adapterWithCatalog(name, ns string, types ...string) *fleetv1.FleetAdapter {
	sa := make([]fleetv1.SupportedAction, 0, len(types))
	for _, ty := range types {
		sa = append(sa, fleetv1.SupportedAction{ActionType: ty})
	}
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     fleetv1.FleetAdapterStatus{SupportedActions: sa},
	}
}

func actionOfType(ns, typ string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionType(typ)},
	}
}

func TestFleetActionSupported_AdvertisedTypeAllowed(t *testing.T) {
	ns := "warehouse-a"
	v := &FleetActionValidator{Reader: newSupportedReader(t, adapterWithCatalog("sim", ns, "Navigate", "Charge"))}
	if _, err := v.ValidateCreate(context.Background(), actionOfType(ns, "Navigate")); err != nil {
		t.Fatalf("advertised type should be allowed, got: %v", err)
	}
}

func TestFleetActionSupported_UnadvertisedTypeRejected(t *testing.T) {
	ns := "warehouse-a"
	v := &FleetActionValidator{Reader: newSupportedReader(t, adapterWithCatalog("sim", ns, "Navigate"))}
	if _, err := v.ValidateCreate(context.Background(), actionOfType(ns, "Frobnicate")); err == nil {
		t.Fatal("unadvertised type should be rejected when a catalog exists")
	}
}

func TestFleetActionSupported_NoCatalogFailsOpen(t *testing.T) {
	ns := "warehouse-a"
	v := &FleetActionValidator{Reader: newSupportedReader(t, adapterWithCatalog("sim", ns))}
	if _, err := v.ValidateCreate(context.Background(), actionOfType(ns, "Anything")); err != nil {
		t.Fatalf("no catalog should fail open, got: %v", err)
	}
}
