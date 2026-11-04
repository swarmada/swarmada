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
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/webhook"
)

// ADR-0053. Two consumers used to answer "what is this namespace's signing
// policy" differently when no SwarmadaConfig existed: the adapter path read absence as
// verification OFF and qualified an unsigned report, while the rollout admission path read
// the same absence as "cannot determine the policy" and refused the write. One namespace,
// one input, two policies — a fault an auditor could only find by reading both.
//
// This test drives BOTH paths against THE SAME config-less namespace and requires them to
// agree. It is deliberately one test rather than two: the defect was never in either
// answer taken alone, it was in the disagreement, and only a test that holds both at once
// can fail when they drift apart again.

const agreeNS = "warehouse-empty"

func agreeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("fleet scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	return s
}

// TestSigningPolicy_BothPathsAgreeWhenNoSwarmadaConfig: neither path may accept unverified
// software in a namespace that has stated no policy.
func TestSigningPolicy_BothPathsAgreeWhenNoSwarmadaConfig(t *testing.T) {
	scheme := agreeScheme(t)
	ctx := context.Background()

	body := `{"adapter":"acme","conformant":true,"contract_version":"1.0.0"}`
	sum := sha256.Sum256([]byte(body))
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now)

	adapter := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: agreeNS},
		Spec: fleetv1.FleetAdapterSpec{
			HeartbeatIntervalSeconds: 10,
			ConformanceReport: &fleetv1.ConformanceReportRef{
				SuiteVersion: "c1-c8",
				ConfigMapRef: "acme-conf",
				Digest:       "sha256:" + hex.EncodeToString(sum[:]),
			},
		},
		Status: fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected, LastHeartbeat: &hb},
	}
	// Digest-verified and conformant, but carrying NO signature — the report that used to
	// pass here purely because the namespace had no policy to check it against.
	reportCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-conf", Namespace: agreeNS},
		Data:       map[string]string{"report.json": body},
	}
	rollout := &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw-roll", Namespace: agreeNS},
		Spec: fleetv1.FirmwareRolloutSpec{
			NewVersion:       "2.0.0",
			FirmwareChecksum: "sha256:" + hex.EncodeToString(sum[:]),
			// no FirmwareSignatureRef
		},
	}

	// One client, one namespace, and NO SwarmadaConfig in it.
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(adapter, reportCM, rollout).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()

	var cfgs fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &cfgs, client.InNamespace(agreeNS)); err != nil {
		t.Fatalf("listing configs: %v", err)
	}
	if len(cfgs.Items) != 0 {
		t.Fatalf("precondition: namespace %q must hold no SwarmadaConfig, found %d", agreeNS, len(cfgs.Items))
	}

	// --- Path 1: the adapter conformance gate ---
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}
	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: agreeNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got fleetv1.FleetAdapter
	if err := c.Get(ctx, types.NamespacedName{Name: "acme", Namespace: agreeNS}, &got); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	adapterRefused := got.Status.Conformance == fleetv1.ConformanceStateFailed

	// --- Path 2: the rollout admission gate ---
	_, rolloutErr := (&webhook.FirmwareRolloutValidator{Client: c}).ValidateCreate(ctx, rollout)
	rolloutRefused := rolloutErr != nil

	// The property under test is AGREEMENT, so it is asserted as agreement first.
	if adapterRefused != rolloutRefused {
		t.Fatalf("the two paths disagree about a namespace with no SwarmadaConfig: "+
			"adapter refused = %v (conformance %s), rollout refused = %v (err %v)",
			adapterRefused, got.Status.Conformance, rolloutRefused, rolloutErr)
	}
	// And then pinned to the SAFE answer, so a future change cannot satisfy agreement by
	// making both fail open.
	if !adapterRefused {
		t.Fatalf("both paths agree, but they agree on ACCEPTING unverified software in a "+
			"namespace with no signing policy: conformance = %s, rollout err = %v",
			got.Status.Conformance, rolloutErr)
	}

	// Each refusal must also say what an operator has to do about it.
	if !strings.Contains(got.Status.Message, "no SwarmadaConfig in namespace") {
		t.Errorf("adapter message %q should name the missing SwarmadaConfig", got.Status.Message)
	}
	if !strings.Contains(rolloutErr.Error(), "no SwarmadaConfig in namespace") {
		t.Errorf("rollout error %q should name the missing SwarmadaConfig", rolloutErr.Error())
	}
}

// The same two paths, in a namespace whose policy EXISTS and says verification is not
// required, must also agree — and there they agree on admitting. Without this, the test
// above would be satisfied by a control plane that simply refuses everything.
func TestSigningPolicy_BothPathsAgreeWhenVerificationIsOff(t *testing.T) {
	scheme := agreeScheme(t)
	ctx := context.Background()

	body := `{"adapter":"acme","conformant":true,"contract_version":"1.0.0"}`
	sum := sha256.Sum256([]byte(body))
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now)

	adapter := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: agreeNS},
		Spec: fleetv1.FleetAdapterSpec{
			HeartbeatIntervalSeconds: 10,
			ConformanceReport: &fleetv1.ConformanceReportRef{
				SuiteVersion: "c1-c8", ConfigMapRef: "acme-conf",
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
			},
		},
		Status: fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected, LastHeartbeat: &hb},
	}
	reportCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-conf", Namespace: agreeNS},
		Data:       map[string]string{"report.json": body},
	}
	rollout := &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw-roll", Namespace: agreeNS},
		Spec: fleetv1.FirmwareRolloutSpec{
			NewVersion: "2.0.0", FirmwareChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(adapter, reportCM, rollout, signingOffConfig(agreeNS)).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()

	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}
	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: agreeNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got fleetv1.FleetAdapter
	if err := c.Get(ctx, types.NamespacedName{Name: "acme", Namespace: agreeNS}, &got); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if got.Status.Conformance != fleetv1.ConformanceStatePassed {
		t.Errorf("adapter conformance = %s, want Passed when the namespace policy says verification is off",
			got.Status.Conformance)
	}
	if _, err := (&webhook.FirmwareRolloutValidator{Client: c}).ValidateCreate(ctx, rollout); err != nil {
		t.Errorf("rollout admission refused an unsigned artifact although the namespace policy "+
			"says verification is off: %v", err)
	}
}
