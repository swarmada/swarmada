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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newNamespaceEstop(t *testing.T, est *fakeEstopper, objs ...client.Object) (*NamespaceEstopReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.Robot{}).WithObjects(objs...).Build()
	return &NamespaceEstopReconciler{Client: c, Estopper: est}, c
}

func nsConfig(trigger, processed string) *fleetv1.SwarmadaConfig {
	ann := map[string]string{}
	if trigger != "" {
		ann[annEstopTriggered] = trigger
	}
	if processed != "" {
		ann[annEstopProcessed] = processed
	}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: zeNS, Name: "swarmada"}}
	if len(ann) > 0 {
		cfg.Annotations = ann
	}
	return cfg
}

func reconcileNsEstop(t *testing.T, r *NamespaceEstopReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "swarmada"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func nsGetConfig(t *testing.T, c client.Client) *fleetv1.SwarmadaConfig {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zeNS, Name: "swarmada"}, cfg); err != nil {
		t.Fatalf("get config: %v", err)
	}
	return cfg
}

// A trigger annotation on the config confirmed-estops every robot in the namespace.
func TestNamespaceEstop_TriggerEstopsAll(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newNamespaceEstop(t, est,
		nsConfig("evacuate", ""),
		zeRobot("amr-1", "z1"), zeRobot("amr-2", "z2"), zeRobot("amr-3", ""),
	)

	reconcileNsEstop(t, r)

	if got := est.names(); len(got) != 3 {
		t.Fatalf("estopped = %v, want all 3 robots", got)
	}
	if got := nsGetConfig(t, c).Annotations[annEstopProcessed]; got != "evacuate" {
		t.Fatalf("processed marker = %q, want the trigger value", got)
	}
}

// The same trigger value does not re-fan-out on a resync.
func TestNamespaceEstop_Idempotent(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newNamespaceEstop(t, est, nsConfig("stop", ""), zeRobot("amr-1", ""), zeRobot("amr-2", ""))

	reconcileNsEstop(t, r)
	reconcileNsEstop(t, r)

	if got := est.names(); len(got) != 2 {
		t.Fatalf("estop fired for %d robots, want 2 (idempotent)", len(got))
	}
}

// Removing the trigger clears every robot in the namespace and drops the marker.
func TestNamespaceEstop_ClearResetsAll(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newNamespaceEstop(t, est, nsConfig("", "stop"), zeRobot("amr-1", ""), zeRobot("amr-2", ""))

	reconcileNsEstop(t, r)

	if got := est.clearedNames(); len(got) != 2 {
		t.Fatalf("cleared = %v, want both robots", got)
	}
	if _, still := nsGetConfig(t, c).Annotations[annEstopProcessed]; still {
		t.Fatal("processed marker not dropped on clear")
	}
}

// No estop annotation is a no-op.
func TestNamespaceEstop_NoAnnotationNoop(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newNamespaceEstop(t, est, nsConfig("", ""), zeRobot("amr-1", ""))

	reconcileNsEstop(t, r)

	if len(est.estopped) != 0 || len(est.cleared) != 0 {
		t.Fatalf("expected no estop action, got estopped=%v cleared=%v", est.estopped, est.cleared)
	}
}
