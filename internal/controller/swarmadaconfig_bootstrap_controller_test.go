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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newBootstrapReconciler(t *testing.T, objs ...client.Object) (*SwarmadaConfigBootstrapReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &SwarmadaConfigBootstrapReconciler{Client: c, Scheme: scheme}, c
}

func reconcileBootstrapZone(t *testing.T, r *SwarmadaConfigBootstrapReconciler, name, ns string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getConfig(t *testing.T, c client.Client, ns string) (*fleetv1.SwarmadaConfig, bool) {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	err := c.Get(context.Background(), types.NamespacedName{Name: SwarmadaConfigName, Namespace: ns}, cfg)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	return cfg, true
}

func bootstrapZone(name, ns string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func TestBootstrap_CreatesConfigOnFirstZone(t *testing.T) {
	r, c := newBootstrapReconciler(t, bootstrapZone("zone-a", "warehouse-a"))
	if _, ok := getConfig(t, c, "warehouse-a"); ok {
		t.Fatal("precondition: config should not exist yet")
	}

	reconcileBootstrapZone(t, r, "zone-a", "warehouse-a")

	if _, ok := getConfig(t, c, "warehouse-a"); !ok {
		t.Fatal("swarmada-config was not auto-created on first FleetZone")
	}
}

func TestBootstrap_NoopWhenConfigExists(t *testing.T) {
	// A pre-existing, operator-authored config must not be overwritten.
	existing := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: "warehouse-a"},
		Spec: fleetv1.SwarmadaConfigSpec{
			Telemetry: fleetv1.SwarmadaTelemetryConfig{Sink: fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkDrop}},
		},
	}
	r, c := newBootstrapReconciler(t, bootstrapZone("zone-a", "warehouse-a"), existing)

	reconcileBootstrapZone(t, r, "zone-a", "warehouse-a")

	cfg, ok := getConfig(t, c, "warehouse-a")
	if !ok {
		t.Fatal("config disappeared")
	}
	if cfg.Spec.Telemetry.Sink.Type != fleetv1.TelemetrySinkDrop {
		t.Fatalf("operator config was overwritten: sink.type = %q, want Drop", cfg.Spec.Telemetry.Sink.Type)
	}
}

func TestBootstrap_IdempotentAcrossManyZones(t *testing.T) {
	r, c := newBootstrapReconciler(t,
		bootstrapZone("zone-a", "warehouse-a"),
		bootstrapZone("zone-b", "warehouse-a"),
	)
	// Reconcile several zone events in the same namespace.
	reconcileBootstrapZone(t, r, "zone-a", "warehouse-a")
	reconcileBootstrapZone(t, r, "zone-b", "warehouse-a")
	reconcileBootstrapZone(t, r, "zone-a", "warehouse-a")

	var list fleetv1.SwarmadaConfigList
	if err := c.List(context.Background(), &list, client.InNamespace("warehouse-a")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly 1 swarmada-config, got %d", len(list.Items))
	}
}

func TestBootstrap_IsolatesNamespaces(t *testing.T) {
	r, c := newBootstrapReconciler(t, bootstrapZone("zone-a", "warehouse-a"))
	reconcileBootstrapZone(t, r, "zone-a", "warehouse-a")

	// A namespace with no FleetZone reconcile must not get a config.
	if _, ok := getConfig(t, c, "warehouse-b"); ok {
		t.Fatal("config created in a namespace with no reconciled FleetZone")
	}
}
