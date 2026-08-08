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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func newCapIngestor(t *testing.T, objs ...client.Object) (*CapabilitiesIngestor, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).
		Build()
	return &CapabilitiesIngestor{Client: c}, c
}

func TestCapabilitiesIngestor_ProjectsCatalog(t *testing.T) {
	ns := "warehouse-a"
	robot := &fleetv1.Robot{
		// The robot-id annotation is the sole wire->Robot join key (ADR-0028); the
		// defaulting webhook guarantees it in production, and a fixture without it
		// exercises a Robot that cannot exist.
		ObjectMeta: metav1.ObjectMeta{
			Name: "amr-1", Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: "amr-1"},
		},
		Spec: fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
	}
	adapter := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "sim-adapter", Namespace: ns}}
	i, c := newCapIngestor(t, robot, adapter)

	snap := &fav1.CapabilitiesSnapshot{
		RobotId: "amr-1",
		SupportedActions: []*fav1.SupportedAction{
			{ActionType: "Navigate", RequiredCapabilities: []string{"navigation.2d"}, Description: "sim: Navigate"},
			{ActionType: "Charge"},
		},
	}
	if err := i.IngestCapabilities(context.Background(), ns, snap); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := &fleetv1.FleetAdapter{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sim-adapter"}, got); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if len(got.Status.SupportedActions) != 2 {
		t.Fatalf("want 2 supported actions, got %d", len(got.Status.SupportedActions))
	}
	if got.Status.SupportedActions[0].ActionType != "Navigate" ||
		len(got.Status.SupportedActions[0].RequiredCapabilities) != 1 {
		t.Fatalf("unexpected projection: %+v", got.Status.SupportedActions[0])
	}

	// RA-1: a second identical ingest is idempotent (no error, catalog unchanged).
	if err := i.IngestCapabilities(context.Background(), ns, snap); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
}

func TestCapabilitiesIngestor_UnknownRobotIsNoop(t *testing.T) {
	i, _ := newCapIngestor(t)
	snap := &fav1.CapabilitiesSnapshot{RobotId: "ghost", SupportedActions: []*fav1.SupportedAction{{ActionType: "Navigate"}}}
	if err := i.IngestCapabilities(context.Background(), "warehouse-a", snap); err != nil {
		t.Fatalf("unknown robot should be a no-op, got: %v", err)
	}
}
