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
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// States the control plane accepts and then does nothing with are told to the
// operator where they look (a condition, an Event), and are written on transition only.

// ── SigningIncomplete on the auto-created SwarmadaConfig ─────────────────────────

func TestEnvtest_SigningIncompleteOnTheAutoCreatedConfig(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "signing-"}}
	if err := envK8s.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() { _ = envK8s.Delete(context.Background(), ns) })

	// The bootstrap reconciler creates the config, exactly as on a first FleetZone.
	boot := &SwarmadaConfigBootstrapReconciler{Client: envK8s, Scheme: envScheme}
	if _, err := boot.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns.Name}}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	patches := &patchCounter{}
	withWatch, ok := envK8s.(client.WithWatch)
	if !ok {
		withWatch = noWatch{envK8s}
	}
	rec := record.NewFakeRecorder(32)
	r := &SwarmadaConfigReconciler{Client: patches.wrap(withWatch), Scheme: envScheme, Recorder: rec}
	key := types.NamespacedName{Namespace: ns.Name, Name: SwarmadaConfigName}
	reconcile := func() *fleetv1.SwarmadaConfig {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		cfg := &fleetv1.SwarmadaConfig{}
		if err := envK8s.Get(ctx, key, cfg); err != nil {
			t.Fatalf("get config: %v", err)
		}
		return cfg
	}
	signingEvents := func() int {
		n := 0
		for _, e := range drainRecorder(rec) {
			if strings.Contains(e, fleetv1.ConditionSigningIncomplete) {
				n++
			}
		}
		return n
	}

	cfg := reconcile()
	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, fleetv1.ConditionSigningIncomplete)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != fleetv1.ReasonNoTrustRoots {
		t.Fatalf("auto-created config: SigningIncomplete = %+v, want True/%s", cond, fleetv1.ReasonNoTrustRoots)
	}
	if n := signingEvents(); n != 1 {
		t.Errorf("SigningIncomplete Warning events on the transition = %d, want 1", n)
	}

	// Unchanged: no status write and no repeated Event (RA-1).
	before := patches.get(ns.Name)
	reconcile()
	reconcile()
	if got := patches.get(ns.Name) - before; got != 0 {
		t.Errorf("status patches on unchanged reconciles = %d, want 0", got)
	}
	if n := signingEvents(); n != 0 {
		t.Errorf("SigningIncomplete Warning events on unchanged reconciles = %d, want 0", n)
	}

	// A trust root turns it False.
	base := cfg.DeepCopy()
	cfg.Spec.Signing.TrustRoots = []fleetv1.SigningTrustRoot{{
		Name: "release-key", SecretRef: fleetv1.SigningSecretKeyRef{Name: "release-key", Key: "cosign.pub"},
	}}
	if err := envK8s.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		t.Fatalf("adding a trust root: %v", err)
	}
	cfg = reconcile()
	cond = apimeta.FindStatusCondition(cfg.Status.Conditions, fleetv1.ConditionSigningIncomplete)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != fleetv1.ReasonTrustRootsConfigured {
		t.Fatalf("after adding a trust root: SigningIncomplete = %+v, want False/%s", cond, fleetv1.ReasonTrustRootsConfigured)
	}
	before = patches.get(ns.Name)
	reconcile()
	if got := patches.get(ns.Name) - before; got != 0 {
		t.Errorf("status patches on an unchanged reconcile after the trust root = %d, want 0", got)
	}
}

// The FleetAdapter's conformance message names the cause, rather than the missing
// signature an operator would then go and add to no effect.
func TestFleetAdapter_ConformanceFailureNamesSigningIncomplete(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	ref := &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "acme-conf", Digest: digestOf(report)}
	incomplete := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: faNS}}

	st := newFAConformanceStatus(t, ref, reportCM("acme-conf", report), incomplete)
	if st.Conformance != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed (fail closed on no trust roots)", st.Conformance)
	}
	if !strings.Contains(st.Message, fleetv1.ConditionSigningIncomplete) || !strings.Contains(st.Message, "trustRoot") {
		t.Errorf("message %q does not name the SigningIncomplete cause", st.Message)
	}
}

// ── A trigger dropped by a suspended ModelPolicy is not silent ─────────────────────

func TestModelPolicy_TriggerDroppedWhileSuspendedEmitsWarning(t *testing.T) {
	mp := policyWithSuspension("nav-gate", 5, true)
	mp.Annotations = map[string]string{triggerAnnotation: `{"version":"v9"}`}
	r, c := newResetReconciler(t, mp)
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nav-gate", Namespace: mpResetNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, pending := getMP(t, c, "nav-gate").Annotations[triggerAnnotation]; pending {
		t.Error("the trigger was not consumed; handling of the annotation must be unchanged")
	}
	events := drainRecorder(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], corev1.EventTypeWarning+" "+reasonTriggerDroppedSuspended) ||
		!strings.Contains(events[0], "reset") {
		t.Errorf("events = %q, want one Warning %s naming the reset", events, reasonTriggerDroppedSuspended)
	}

	// Nothing pending: no Event.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nav-gate", Namespace: mpResetNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if events := drainRecorder(rec); len(events) != 0 {
		t.Errorf("events with no pending trigger = %q, want none", events)
	}
}

// ── FleetAdapter spec fields no controller acts on ────────────────────────────────

func ignoredFieldsFixture(t *testing.T, spec fleetv1.FleetAdapterSpec) (*FleetAdapterReconciler, client.Client, *int, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	fa := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS}, Spec: spec}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fa, signingOffConfig(faNS)).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*fleetv1.FleetAdapter); ok {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	rec := record.NewFakeRecorder(16)
	now := time.Unix(1000, 0)
	return &FleetAdapterReconciler{Client: c, Scheme: scheme, Recorder: rec, now: func() time.Time { return now }}, c, &writes, rec
}

func TestFleetAdapter_SpecFieldsIgnoredConditionIsTransitionOnly(t *testing.T) {
	r, c, writes, rec := ignoredFieldsFixture(t, fleetv1.FleetAdapterSpec{
		HeartbeatIntervalSeconds: 10,
		ProtocolVersion:          "fleet_adapter.v2",
		TLSSecretRef:             "acme-client-tls",
	})
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS}}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	fa := getFA(t, c)
	cond := apimeta.FindStatusCondition(fa.Status.Conditions, fleetv1.ConditionSpecFieldsIgnored)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("SpecFieldsIgnored = %+v, want True", cond)
	}
	for _, want := range []string{"spec.protocolVersion", "spec.tlsSecretRef", "has no effect in this release"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not contain %q", cond.Message, want)
		}
	}
	if *writes != 1 {
		t.Errorf("status writes over three reconciles = %d, want 1 (RA-1)", *writes)
	}
	var warnings int
	for _, e := range drainRecorder(rec) {
		if strings.Contains(e, fleetv1.ConditionSpecFieldsIgnored) {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("SpecFieldsIgnored Warning events over three reconciles = %d, want 1", warnings)
	}
}

// The schema defaults spec.protocolVersion to the protocol the plane speaks, so the
// default is not reported: every adapter would carry the condition otherwise.
func TestFleetAdapter_DefaultProtocolVersionIsNotReported(t *testing.T) {
	r, c, _, rec := ignoredFieldsFixture(t, fleetv1.FleetAdapterSpec{
		HeartbeatIntervalSeconds: 10, ProtocolVersion: "fleet_adapter.v1",
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cond := apimeta.FindStatusCondition(getFA(t, c).Status.Conditions, fleetv1.ConditionSpecFieldsIgnored)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("SpecFieldsIgnored = %+v, want False", cond)
	}
	for _, e := range drainRecorder(rec) {
		if strings.Contains(e, fleetv1.ConditionSpecFieldsIgnored) {
			t.Errorf("unexpected event %q", e)
		}
	}
}
