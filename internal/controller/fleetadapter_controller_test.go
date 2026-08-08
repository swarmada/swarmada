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
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
)

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newFAConformance builds a Connected adapter with a conformanceReport plus its
// report ConfigMap, and reconciles once. Returns the resulting conformance state.
func newFAConformance(t *testing.T, report *fleetv1.ConformanceReportRef, cm *corev1.ConfigMap) fleetv1.ConformanceState {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now)
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10, ConformanceReport: report},
		Status:     fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected, LastHeartbeat: &hb},
	}
	objs := []client.Object{fa}
	if cm != nil {
		objs = append(objs, cm)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return getFA(t, c).Status.Conformance
}

func reportCM(name, content string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: faNS},
		Data:       map[string]string{"report.json": content},
	}
}

// A digest-verified, conformant report yields Conformance: Passed.
func TestFleetAdapter_ConformancePassed(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	ref := &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "acme-conf", Digest: digestOf(report)}
	if got := newFAConformance(t, ref, reportCM("acme-conf", report)); got != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed", got)
	}
}

// A digest mismatch (report altered or wrong digest pinned) fails closed to Failed
// — an unverifiable report is NEVER Passed.
func TestFleetAdapter_ConformanceDigestMismatchFailsClosed(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	ref := &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "acme-conf", Digest: digestOf("a different report")}
	if got := newFAConformance(t, ref, reportCM("acme-conf", report)); got != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed (digest mismatch must never be Passed)", got)
	}
}

// A verified but non-conformant report is Failed.
func TestFleetAdapter_ConformanceNonConformantFailed(t *testing.T) {
	report := `{"adapter":"acme","conformant":false}`
	ref := &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "acme-conf", Digest: digestOf(report)}
	if got := newFAConformance(t, ref, reportCM("acme-conf", report)); got != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed", got)
	}
}

// A missing report ConfigMap fails closed to Failed.
func TestFleetAdapter_ConformanceMissingConfigMapFailsClosed(t *testing.T) {
	ref := &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "absent", Digest: digestOf("x")}
	if got := newFAConformance(t, ref, nil); got != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed (missing report must never be Passed)", got)
	}
}

// No conformanceReport referenced yields Unknown.
func TestFleetAdapter_ConformanceNoReportIsUnknown(t *testing.T) {
	if got := newFAConformance(t, nil, nil); got != fleetv1.ConformanceStateUnknown {
		t.Fatalf("conformance = %s, want Unknown", got)
	}
}

const faNS = "warehouse-a"

// compatibleNegotiation is the handshake outcome these fixtures assume: an adapter reporting the
// contract version this build implements. Spelled out as a helper because the ADR-0032 rejection
// path turns an incompatible negotiation into phase Rejected, so a test that means "connected" has
// to say which contract was agreed (see fleetadapter_contract_gate_test.go for the other side).
func compatibleNegotiation() controlstream.Negotiation {
	return controlstream.Negotiation{
		ProtocolVersion:    "fleet_adapter.v1",
		ContractVersion:    contract.Version,
		ContractCompatible: true,
	}
}

func faIdentity() controlstream.TLSIdentity {
	return controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}
}

func newFAReconciler(t *testing.T, now time.Time, phase fleetv1.FleetAdapterPhase, lastHB *metav1.Time) (*FleetAdapterReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10},
		Status:     fleetv1.FleetAdapterStatus{Phase: phase, LastHeartbeat: lastHB},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fa).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()
	return &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}, c
}

func getFA(t *testing.T, c client.Client) *fleetv1.FleetAdapter {
	t.Helper()
	fa := &fleetv1.FleetAdapter{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "acme", Namespace: faNS}, fa); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	return fa
}

// A verified connect sets phase Connected + lastHeartbeat + negotiated version.
func TestFleetAdapter_ConnectedSetsPhase(t *testing.T) {
	now := time.Unix(1000, 0)
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhasePending, nil)

	r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())

	fa := getFA(t, c)
	if fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
		t.Fatalf("phase = %s, want Connected", fa.Status.Phase)
	}
	if fa.Status.LastHeartbeat == nil || fa.Status.NegotiatedProtocolVersion != "fleet_adapter.v1" {
		t.Fatalf("status = %+v", fa.Status)
	}
}

// Stream loss moves a Connected adapter to Disconnected.
func TestFleetAdapter_DisconnectedOnLoss(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now)
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseConnected, &hb)

	r.AdapterDisconnected(context.Background(), faIdentity())

	if fa := getFA(t, c); fa.Status.Phase != fleetv1.FleetAdapterPhaseDisconnected {
		t.Fatalf("phase = %s, want Disconnected", fa.Status.Phase)
	}
}

// The staleness backstop disconnects an adapter whose heartbeat went stale.
func TestFleetAdapter_StaleHeartbeatDisconnects(t *testing.T) {
	now := time.Unix(1000, 0)
	stale := metav1.NewTime(now.Add(-40 * time.Second)) // > 10s × 3
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseConnected, &stale)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fa := getFA(t, c); fa.Status.Phase != fleetv1.FleetAdapterPhaseDisconnected {
		t.Fatalf("phase = %s, want Disconnected (stale liveness)", fa.Status.Phase)
	}
}

// A fresh heartbeat within the window does NOT disconnect the adapter.
func TestFleetAdapter_FreshHeartbeatStaysConnected(t *testing.T) {
	now := time.Unix(1000, 0)
	fresh := metav1.NewTime(now.Add(-5 * time.Second))
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseConnected, &fresh)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fa := getFA(t, c); fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
		t.Fatalf("phase = %s, want still Connected", fa.Status.Phase)
	}
}

// RA-1: a repeated identical connect (frozen clock) produces NO second status
// write — the resourceVersion is unchanged.
func TestFleetAdapter_NoSpuriousWrite(t *testing.T) {
	now := time.Unix(1000, 0)
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhasePending, nil)

	r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation())
	rv1 := getFA(t, c).ResourceVersion
	r.AdapterConnected(context.Background(), faIdentity(), compatibleNegotiation()) // identical
	rv2 := getFA(t, c).ResourceVersion

	if rv1 != rv2 {
		t.Fatalf("identical connect wrote status again (rv %s → %s) — RA-1 spurious write", rv1, rv2)
	}
}

// A presence event for a FleetAdapter that does not exist is a safe no-op.
func TestFleetAdapter_MissingCRIsNoOp(t *testing.T) {
	now := time.Unix(1000, 0)
	r, _ := newFAReconciler(t, now, fleetv1.FleetAdapterPhasePending, nil)
	// No panic / no error for an unknown adapter.
	r.AdapterConnected(context.Background(), controlstream.TLSIdentity{AdapterName: "ghost", Namespace: faNS, Verified: true}, compatibleNegotiation())
}
