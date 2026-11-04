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
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// SWARMADA_CONFIG_MODIFIED (RFC-0001 #safety-required-events) records a change to a
// SwarmadaConfig's spec that the API server stored. The controller writes it when it observes
// a generation it has not observed before, and names itself: the Kubernetes API server's audit
// log records who wrote the change. A create is not a modification; a change to anything but
// spec, a server-side dry run, and an update the API server refused have no generation of their
// own and record nothing.

// configModified returns the SWARMADA_CONFIG_MODIFIED entries in spy.
func configModified(spy *auditSpy) []audit.Entry {
	var out []audit.Entry
	for _, e := range spy.entries {
		if e.EventType == audit.EventSwarmadaConfigMod {
			out = append(out, e)
		}
	}
	return out
}

func wantConfigModified(t *testing.T, spy *auditSpy, want int) {
	t.Helper()
	got := configModified(spy)
	if len(got) != want {
		t.Fatalf("SWARMADA_CONFIG_MODIFIED entries = %d (%+v), want %d", len(got), got, want)
	}
	for i, e := range got {
		if e.Actor != (audit.Actor{Type: audit.ActorServiceAccount, Identity: "swarmadaconfig-controller"}) {
			t.Errorf("entry %d actor = %+v, want the controller", i, e.Actor)
		}
		if e.Action != "update" || e.Outcome != audit.OutcomeAllowed || e.Resource.Kind != "SwarmadaConfig" ||
			e.Resource.Name != SwarmadaConfigName || e.Namespace != e.Resource.Namespace {
			t.Errorf("entry %d does not identify the stored change: %+v", i, e)
		}
	}
}

// envConfig creates the namespace's SwarmadaConfig through the API server.
func envConfig(t *testing.T, ns string) *fleetv1.SwarmadaConfig {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: ns}}
	if err := envK8s.Create(context.Background(), cfg); err != nil {
		t.Fatalf("creating the SwarmadaConfig: %v", err)
	}
	return cfg
}

// editConfig writes one change to the stored config, as an operator would.
func editConfig(t *testing.T, ns string, mutate func(*fleetv1.SwarmadaConfig), opts ...client.UpdateOption) {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: SwarmadaConfigName}, cfg); err != nil {
		t.Fatalf("reading the SwarmadaConfig: %v", err)
	}
	mutate(cfg)
	if err := envK8s.Update(context.Background(), cfg, opts...); err != nil {
		t.Fatalf("updating the SwarmadaConfig: %v", err)
	}
}

func reconcileConfigOnce(t *testing.T, r *SwarmadaConfigReconciler, ns string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: SwarmadaConfigName}})
	return err
}

func raiseTelemetryInterval(c *fleetv1.SwarmadaConfig) { c.Spec.Health.TelemetryIntervalSeconds++ }

func TestEnvtest_ConfigModifiedIsRecordedForAStoredSpecChange(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	spy := &auditSpy{}
	r := &SwarmadaConfigReconciler{Client: envK8s, Scheme: envScheme, Audit: spy}
	envConfig(t, ns)

	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 0) // the create

	editConfig(t, ns, raiseTelemetryInterval)
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 1)

	// A second pass over the same generation records nothing more.
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 1)

	// An annotation or a label is not a change to spec.
	editConfig(t, ns, func(c *fleetv1.SwarmadaConfig) {
		c.Annotations = map[string]string{"example.com/note": "x"}
		c.Labels = map[string]string{"team": "ops"}
	})
	// A server-side dry run stores nothing.
	editConfig(t, ns, raiseTelemetryInterval, client.DryRunAll)
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 1)
}

func TestEnvtest_ConfigModifiedCoversEditsMadeBeforeAPass(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	spy := &auditSpy{}
	r := &SwarmadaConfigReconciler{Client: envK8s, Scheme: envScheme, Audit: spy}
	envConfig(t, ns)
	// Two edits before the controller's first pass (the manager was down): one entry.
	editConfig(t, ns, raiseTelemetryInterval)
	editConfig(t, ns, raiseTelemetryInterval)
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 1)
	// Two more between passes: one entry for both.
	editConfig(t, ns, raiseTelemetryInterval)
	editConfig(t, ns, raiseTelemetryInterval)
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	wantConfigModified(t, spy, 2)
}

// statusFailsOnce refuses the first status patch, as a lost write would.
type statusFailsOnce struct {
	client.Client
	failed bool
}

func (c *statusFailsOnce) Status() client.SubResourceWriter {
	return &statusFailsOnceWriter{SubResourceWriter: c.Client.Status(), c: c}
}

type statusFailsOnceWriter struct {
	client.SubResourceWriter
	c *statusFailsOnce
}

func (w *statusFailsOnceWriter) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if !w.c.failed {
		w.c.failed = true
		return errors.New("the status write was lost")
	}
	return w.SubResourceWriter.Patch(ctx, obj, p, opts...)
}

func TestEnvtest_ConfigModifiedIsRecordedAgainAfterALostStatusWrite(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	spy := &auditSpy{}
	r := &SwarmadaConfigReconciler{Client: &statusFailsOnce{Client: envK8s}, Scheme: envScheme, Audit: spy}
	envConfig(t, ns)
	editConfig(t, ns, raiseTelemetryInterval)
	if err := reconcileConfigOnce(t, r, ns); err == nil {
		t.Fatal("fixture: the first status write was meant to be lost")
	}
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	if err := reconcileConfigOnce(t, r, ns); err != nil {
		t.Fatal(err)
	}
	// At least once: the change is recorded, and recorded again after the lost write.
	wantConfigModified(t, spy, 2)
}

// failingAudit refuses every record.
type failingAudit struct{ calls int }

func (f *failingAudit) Record(e audit.Entry) (audit.Entry, error) {
	f.calls++
	return e, errors.New("sink unavailable")
}

func storedConfig(gen, observed int64) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: "warehouse-a", Generation: gen},
		Status:     fleetv1.SwarmadaConfigStatus{ObservedGeneration: observed},
	}
}

func TestConfigModified_AFailingSinkDoesNotHoldTheStatus(t *testing.T) {
	sc := robotAuditScheme(t)
	c := fake.NewClientBuilder().WithScheme(sc).WithStatusSubresource(&fleetv1.SwarmadaConfig{}).
		WithObjects(storedConfig(2, 1)).Build()
	sink := &failingAudit{}
	r := &SwarmadaConfigReconciler{Client: c, Scheme: sc, Audit: sink}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "warehouse-a", Name: SwarmadaConfigName}}); err != nil {
		t.Fatalf("a failing audit sink failed the reconcile: %v", err)
	}
	if sink.calls != 1 {
		t.Errorf("record attempts = %d, want 1", sink.calls)
	}
	var got fleetv1.SwarmadaConfig
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "warehouse-a", Name: SwarmadaConfigName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want 2: the status write waited on the audit sink", got.Status.ObservedGeneration)
	}
}

func TestConfigModified_NilRecorderIsSafe(t *testing.T) {
	sc := robotAuditScheme(t)
	c := fake.NewClientBuilder().WithScheme(sc).WithStatusSubresource(&fleetv1.SwarmadaConfig{}).
		WithObjects(storedConfig(2, 1)).Build()
	r := &SwarmadaConfigReconciler{Client: c, Scheme: sc}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "warehouse-a", Name: SwarmadaConfigName}}); err != nil {
		t.Fatalf("Reconcile with nil Audit: %v", err)
	}
}
