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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newConfigReconciler(t *testing.T, cfg *fleetv1.SwarmadaConfig) (*SwarmadaConfigReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cfg).
		WithStatusSubresource(&fleetv1.SwarmadaConfig{}).
		Build()
	rec := record.NewFakeRecorder(8)
	return &SwarmadaConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}, c, rec
}

func reconcileConfig(t *testing.T, r *SwarmadaConfigReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "swarmada-config", Namespace: "warehouse-a"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getConfigCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "swarmada-config", Namespace: "warehouse-a"}, cfg); err != nil {
		t.Fatalf("get config: %v", err)
	}
	return meta.FindStatusCondition(cfg.Status.Conditions, fleetv1.ConditionTelemetrySinkUnconfigured)
}

func configWithSink(sinkType fleetv1.TelemetrySinkType) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: "warehouse-a"},
		Spec:       fleetv1.SwarmadaConfigSpec{Telemetry: fleetv1.SwarmadaTelemetryConfig{Sink: fleetv1.TelemetrySink{Type: sinkType}}},
	}
}

func TestSwarmadaConfig_UnsetSinkRaisesCondition(t *testing.T) {
	r, c, rec := newConfigReconciler(t, configWithSink(fleetv1.TelemetrySinkUnset))
	reconcileConfig(t, r)

	cond := getConfigCondition(t, c)
	if cond == nil {
		t.Fatal("TelemetrySinkUnconfigured condition not set")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != fleetv1.ReasonSinkNotConfigured {
		t.Fatalf("condition = %s/%s, want True/SinkNotConfigured", cond.Status, cond.Reason)
	}
	// A Warning event must be emitted while unconfigured (§9.3.7).
	select {
	case <-rec.Events:
	default:
		t.Fatal("expected a TelemetrySinkUnconfigured Warning event")
	}
}

func TestSwarmadaConfig_DropSinkClearsCondition(t *testing.T) {
	// Drop is the operator's explicit opt-in: the condition must be False and no
	// Warning event emitted.
	r, c, rec := newConfigReconciler(t, configWithSink(fleetv1.TelemetrySinkDrop))
	reconcileConfig(t, r)

	cond := getConfigCondition(t, c)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != fleetv1.ReasonSinkConfigured {
		t.Fatalf("condition = %+v, want False/SinkConfigured", cond)
	}
	select {
	case e := <-rec.Events:
		t.Fatalf("unexpected event for Drop sink: %q", e)
	default:
	}
}

func TestSwarmadaConfig_RealSinkClearsCondition(t *testing.T) {
	r, c, _ := newConfigReconciler(t, configWithSink(fleetv1.TelemetrySinkPrometheusRemoteWrite))
	reconcileConfig(t, r)

	cond := getConfigCondition(t, c)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != fleetv1.ReasonSinkConfigured {
		t.Fatalf("condition = %+v, want False/SinkConfigured", cond)
	}
}

func TestSwarmadaConfig_TransitionUnsetToConfigured(t *testing.T) {
	r, c, _ := newConfigReconciler(t, configWithSink(fleetv1.TelemetrySinkUnset))
	reconcileConfig(t, r)
	if cond := getConfigCondition(t, c); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected True after unset reconcile, got %+v", cond)
	}

	// Operator configures a real sink; the next reconcile must clear the condition.
	cfg := &fleetv1.SwarmadaConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "swarmada-config", Namespace: "warehouse-a"}, cfg); err != nil {
		t.Fatalf("get: %v", err)
	}
	cfg.Spec.Telemetry.Sink.Type = fleetv1.TelemetrySinkVictoriaMetrics
	if err := c.Update(context.Background(), cfg); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcileConfig(t, r)
	if cond := getConfigCondition(t, c); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected False after configuring sink, got %+v", cond)
	}
}
