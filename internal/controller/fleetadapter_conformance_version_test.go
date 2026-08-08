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
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ADR-0032 "Version-bound conformance": status.conformanceContractVersion records the contract
// version the verified result was earned against, so a Passed earned against one revision of the
// contract is no longer indistinguishable from one earned against any other.
//
// Conformance stays SELF-CERTIFIED (ADR-0007) — these tests only ever exercise the CONSUMER side:
// the controller reads an already-attested report. Nothing here issues a result.

const cvNS = "warehouse-a"

// cvReport builds a report body. An empty contractVersion omits the key entirely, which is exactly
// what a report from a pre-versioning harness looks like.
func cvReport(conformant bool, contractVersion string) string {
	m := map[string]any{
		"adapter":          "sim",
		"protocol_version": "fleet_adapter.v1",
		"conformant":       conformant,
	}
	if contractVersion != "" {
		m["contract_version"] = contractVersion
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func cvDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// cvFixture wires an adapter whose spec pins the report's digest, plus the ConfigMap holding it.
// digestOverride != "" pins a WRONG digest, simulating an altered report.
func cvFixture(t *testing.T, body, digestOverride string, extra ...client.Object) (*FleetAdapterReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add fleet scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	digest := cvDigest(body)
	if digestOverride != "" {
		digest = digestOverride
	}
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: cvNS},
		Spec: fleetv1.FleetAdapterSpec{
			ConformanceReport: &fleetv1.ConformanceReportRef{
				SuiteVersion: "c1-c8", ConfigMapRef: "report-cm", Digest: digest,
			},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "report-cm", Namespace: cvNS},
		Data:       map[string]string{"report.json": body},
	}
	objs := append([]client.Object{fa, cm}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()
	return &FleetAdapterReconciler{Client: c, Scheme: scheme}, c
}

func cvReconcile(t *testing.T, r *FleetAdapterReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "adapter-a", Namespace: cvNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func cvGet(t *testing.T, c client.Client) fleetv1.FleetAdapter {
	t.Helper()
	var fa fleetv1.FleetAdapter
	if err := c.Get(context.Background(), types.NamespacedName{Name: "adapter-a", Namespace: cvNS}, &fa); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	return fa
}

// The version the harness stamped is recorded beside the result.
func TestConformanceVersion_RecordedFromVerifiedReport(t *testing.T) {
	r, c := cvFixture(t, cvReport(true, "1.0.0"), "")
	cvReconcile(t, r)
	fa := cvGet(t, c)
	if fa.Status.Conformance != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed", fa.Status.Conformance)
	}
	if fa.Status.ConformanceContractVersion != "1.0.0" {
		t.Errorf("contract version = %q, want 1.0.0", fa.Status.ConformanceContractVersion)
	}
	if want := "contract 1.0.0"; !strings.Contains(fa.Status.Message, want) {
		t.Errorf("message %q should mention %q so an operator can see the binding", fa.Status.Message, want)
	}
}

// A report from a pre-versioning harness carries no contract_version. That is recorded as UNKNOWN
// (empty) and said so in the message — it must NOT flip conformance to Failed, which would silently
// invalidate every attestation made before the field existed.
func TestConformanceVersion_AbsentIsUnknownNotFailure(t *testing.T) {
	r, c := cvFixture(t, cvReport(true, ""), "")
	cvReconcile(t, r)
	fa := cvGet(t, c)
	if fa.Status.Conformance != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed (a missing contract_version is not a failure here)",
			fa.Status.Conformance)
	}
	if fa.Status.ConformanceContractVersion != "" {
		t.Errorf("contract version = %q, want empty (never inferred)", fa.Status.ConformanceContractVersion)
	}
	if !strings.Contains(fa.Status.Message, "no contract_version") {
		t.Errorf("message %q should say the report carries no contract version", fa.Status.Message)
	}
}

// The version may only come from a report whose digest verified: an altered report yields Failed AND
// no recorded version, so a version never launders an untrusted body.
func TestConformanceVersion_NotRecordedFromUnverifiedReport(t *testing.T) {
	r, c := cvFixture(t, cvReport(true, "9.9.9"), "sha256:"+hex.EncodeToString(make([]byte, 32)))
	cvReconcile(t, r)
	fa := cvGet(t, c)
	if fa.Status.Conformance != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed on a digest mismatch", fa.Status.Conformance)
	}
	if fa.Status.ConformanceContractVersion != "" {
		t.Errorf("contract version = %q recorded from an UNVERIFIED report", fa.Status.ConformanceContractVersion)
	}
}

// A non-conformant report records no version either — there is no result to bind one to.
func TestConformanceVersion_NotRecordedWhenNonConformant(t *testing.T) {
	r, c := cvFixture(t, cvReport(false, "1.0.0"), "")
	cvReconcile(t, r)
	fa := cvGet(t, c)
	if fa.Status.Conformance != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed", fa.Status.Conformance)
	}
	if fa.Status.ConformanceContractVersion != "" {
		t.Errorf("contract version = %q recorded for a non-conformant result", fa.Status.ConformanceContractVersion)
	}
}

// A stale version must not outlive the result it belonged to: once the report stops verifying, the
// recorded version is CLEARED rather than left behind next to a Failed.
func TestConformanceVersion_ClearedWhenReportStopsVerifying(t *testing.T) {
	r, c := cvFixture(t, cvReport(true, "1.0.0"), "")
	cvReconcile(t, r)
	if got := cvGet(t, c).Status.ConformanceContractVersion; got != "1.0.0" {
		t.Fatalf("precondition: contract version = %q, want 1.0.0", got)
	}
	// The report body is altered under the pinned digest (tampering).
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: "report-cm", Namespace: cvNS}, &cm); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	cm.Data["report.json"] = cvReport(true, "6.6.6")
	if err := c.Update(context.Background(), &cm); err != nil {
		t.Fatalf("update cm: %v", err)
	}
	cvReconcile(t, r)
	fa := cvGet(t, c)
	if fa.Status.Conformance != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed after tampering", fa.Status.Conformance)
	}
	if fa.Status.ConformanceContractVersion != "" {
		t.Errorf("contract version = %q survived the result it was earned with", fa.Status.ConformanceContractVersion)
	}
}

// RA-1 / transition-only: recording the version must not make the controller write status on every
// reconcile. Two reconciles over an unchanged report produce exactly ONE status write.
func TestConformanceVersion_TransitionOnlyWrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add fleet scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	body := cvReport(true, "1.0.0")
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: cvNS},
		Spec: fleetv1.FleetAdapterSpec{ConformanceReport: &fleetv1.ConformanceReportRef{
			SuiteVersion: "c1-c8", ConfigMapRef: "report-cm", Digest: cvDigest(body)}},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "report-cm", Namespace: cvNS},
		Data:       map[string]string{"report.json": body},
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fa, cm).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isAdapter := obj.(*fleetv1.FleetAdapter); isAdapter {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme}

	cvReconcile(t, r)
	cvReconcile(t, r)
	if writes != 1 {
		t.Errorf("status writes = %d, want 1 (the version must be recorded transition-only, RA-1)", writes)
	}
	if got := cvGet(t, c).Status.ConformanceContractVersion; got != "1.0.0" {
		t.Errorf("contract version = %q, want 1.0.0", got)
	}
}

// A re-attestation against a NEW contract version updates the recorded value (and does write).
func TestConformanceVersion_UpdatesOnReAttestation(t *testing.T) {
	r, c := cvFixture(t, cvReport(true, "1.0.0"), "")
	cvReconcile(t, r)
	if got := cvGet(t, c).Status.ConformanceContractVersion; got != "1.0.0" {
		t.Fatalf("precondition: %q", got)
	}
	// Re-run against 1.1.0: a new body AND the newly pinned digest, as a registry PR would carry.
	body := cvReport(true, "1.1.0")
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: "report-cm", Namespace: cvNS}, &cm); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	cm.Data["report.json"] = body
	if err := c.Update(context.Background(), &cm); err != nil {
		t.Fatalf("update cm: %v", err)
	}
	fa := cvGet(t, c)
	fa.Spec.ConformanceReport.Digest = cvDigest(body)
	if err := c.Update(context.Background(), &fa); err != nil {
		t.Fatalf("update adapter: %v", err)
	}
	cvReconcile(t, r)
	got := cvGet(t, c)
	if got.Status.Conformance != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed", got.Status.Conformance)
	}
	if got.Status.ConformanceContractVersion != "1.1.0" {
		t.Errorf("contract version = %q, want 1.1.0 after re-attestation", got.Status.ConformanceContractVersion)
	}
}
