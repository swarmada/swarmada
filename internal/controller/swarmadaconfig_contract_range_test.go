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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
)

// ADR-0032: the control plane ADVERTISES the contract-version range it accepts on
// SwarmadaConfig.status.supportedContractRange, populated from the compiled-in contract version.

const crNS = "warehouse-a"

func crConfig() *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: crNS},
		// A real sink keeps the TelemetrySinkUnconfigured path (and its requeue) out of the way.
		Spec: fleetv1.SwarmadaConfigSpec{
			Telemetry: fleetv1.SwarmadaTelemetryConfig{
				Sink: fleetv1.TelemetrySink{Type: fleetv1.TelemetrySinkDrop},
			},
		},
	}
}

func crReconcile(t *testing.T, r *SwarmadaConfigReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "swarmada-config", Namespace: crNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func crGet(t *testing.T, c client.Client) fleetv1.SwarmadaConfig {
	t.Helper()
	var cfg fleetv1.SwarmadaConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: "swarmada-config", Namespace: crNS}, &cfg); err != nil {
		t.Fatalf("get config: %v", err)
	}
	return cfg
}

// The range is advertised from the compiled-in version — not left empty for an operator to fill.
func TestContractRange_AdvertisedOnReconcile(t *testing.T) {
	r, c, _ := newConfigReconciler(t, crConfig())
	crReconcile(t, r)
	if got, want := crGet(t, c).Status.SupportedContractRange, contract.SupportedRange(); got != want {
		t.Fatalf("supportedContractRange = %q, want %q (advertised from contract.Version %s)",
			got, want, contract.Version)
	}
}

// It is ADVERTISED, not configured: an operator edit is restored, because the compiled-in version is
// the only source of truth for what this build can actually drive.
func TestContractRange_SelfHealsAnOperatorEdit(t *testing.T) {
	r, c, _ := newConfigReconciler(t, crConfig())
	crReconcile(t, r)

	cfg := crGet(t, c)
	cfg.Status.SupportedContractRange = ">=0.0.0 <99.0.0" // an operator "widening" it by hand
	if err := c.Status().Update(context.Background(), &cfg); err != nil {
		t.Fatalf("status update: %v", err)
	}
	crReconcile(t, r)
	if got, want := crGet(t, c).Status.SupportedContractRange, contract.SupportedRange(); got != want {
		t.Errorf("supportedContractRange = %q after an operator edit, want it restored to %q", got, want)
	}
}

// RA-1 / transition-only: advertising the range must not make the controller write status on every
// reconcile. Two reconciles over an unchanged config produce exactly ONE status write.
func TestContractRange_TransitionOnlyWrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crConfig()).
		WithStatusSubresource(&fleetv1.SwarmadaConfig{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isCfg := obj.(*fleetv1.SwarmadaConfig); isCfg {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &SwarmadaConfigReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	crReconcile(t, r)
	crReconcile(t, r)
	if writes != 1 {
		t.Errorf("status writes = %d, want 1 (the advertised range must be transition-only, RA-1)", writes)
	}
	if got, want := crGet(t, c).Status.SupportedContractRange, contract.SupportedRange(); got != want {
		t.Errorf("supportedContractRange = %q, want %q", got, want)
	}
}

// Advertising the range must not disturb what the controller already reports: the
// TelemetrySinkUnconfigured condition and observedGeneration still land.
func TestContractRange_LeavesExistingStatusIntact(t *testing.T) {
	cfg := crConfig()
	cfg.Spec.Telemetry.Sink.Type = fleetv1.TelemetrySinkUnset // drive the condition to True
	r, c, _ := newConfigReconciler(t, cfg)
	crReconcile(t, r)

	got := crGet(t, c)
	if got.Status.SupportedContractRange == "" {
		t.Error("supportedContractRange was not advertised")
	}
	var found bool
	for _, cond := range got.Status.Conditions {
		if cond.Type == fleetv1.ConditionTelemetrySinkUnconfigured {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("TelemetrySinkUnconfigured = %s, want True for an unset sink", cond.Status)
			}
		}
	}
	if !found {
		t.Error("the TelemetrySinkUnconfigured condition disappeared")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}
