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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
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
)

// signBundle signs content with a fresh ed25519 key, returning the "bundle:<base64>"
// signature and the PKIX public-key PEM (the trust root that verifies it).
func signBundle(t *testing.T, content string) (bundleSig string, pubPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return "bundle:" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(content))), pubPEM
}

// reconcileSignedConformance builds a Connected adapter + a report ConfigMap (report
// + optional signature key), a SwarmadaConfig with the given require flag and a
// trust-root Secret holding pubPEM, reconciles once, and returns the conformance.
func reconcileSignedConformance(t *testing.T, content, digest, sig string, pubPEM []byte, require bool) fleetv1.ConformanceState {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now)

	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Spec: fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10,
			ConformanceReport: &fleetv1.ConformanceReportRef{SuiteVersion: "v1.2.0", ConfigMapRef: "acme-conf", Digest: digest}},
		Status: fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected, LastHeartbeat: &hb},
	}
	cmData := map[string]string{"report.json": content}
	if sig != "" {
		cmData["signature"] = sig
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "acme-conf", Namespace: faNS}, Data: cmData}
	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: faNS},
		Spec: fleetv1.SwarmadaConfigSpec{Signing: fleetv1.SwarmadaSigningConfig{
			RequireSignatureVerification: require,
			TrustRoots: []fleetv1.SigningTrustRoot{{Name: "conformance-authority",
				SecretRef: fleetv1.SigningSecretKeyRef{Name: "ca-secret", Key: "pub.pem"}}},
		}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca-secret", Namespace: faNS},
		Data: map[string][]byte{"pub.pem": pubPEM}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fa, cm, cfg, secret).
		WithStatusSubresource(&fleetv1.FleetAdapter{}).Build()
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return getFA(t, c).Status.Conformance
}

func TestFleetAdapter_SignatureVerifiedPasses(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	sig, pub := signBundle(t, report)
	if got := reconcileSignedConformance(t, report, digestOf(report), sig, pub, true); got != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed (digest + signature verified)", got)
	}
}

// Enforcement on + no signature in the ConfigMap fails closed.
func TestFleetAdapter_SignatureRequiredButMissingFailsClosed(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	_, pub := signBundle(t, report)
	if got := reconcileSignedConformance(t, report, digestOf(report), "", pub, true); got != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed (required signature absent)", got)
	}
}

// A signature that does not verify against the trust root fails closed, even though
// the digest matches — authenticity is independent of integrity.
func TestFleetAdapter_SignatureWrongSignerFailsClosed(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	sig, _ := signBundle(t, report) // signed by key A
	_, otherPub := signBundle(t, report)
	if got := reconcileSignedConformance(t, report, digestOf(report), sig, otherPub, true); got != fleetv1.ConformanceStateFailed {
		t.Fatalf("conformance = %s, want Failed (signature from an untrusted key)", got)
	}
}

// Enforcement OFF: an unsigned report still verifies by digest alone (prior behaviour).
func TestFleetAdapter_SignatureNotRequiredDigestOnly(t *testing.T) {
	report := `{"adapter":"acme","conformant":true}`
	_, pub := signBundle(t, report)
	if got := reconcileSignedConformance(t, report, digestOf(report), "", pub, false); got != fleetv1.ConformanceStatePassed {
		t.Fatalf("conformance = %s, want Passed (signature not required)", got)
	}
}

// ── Degraded intermediate phase ───────────────────────────────────────────────

func reconcilePhase(t *testing.T, r *FleetAdapterReconciler, c client.Client) fleetv1.FleetAdapterPhase {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "acme", Namespace: faNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return getFA(t, c).Status.Phase
}

// The KEY new behaviour: missing at least one but fewer than the threshold of
// intervals moves a Connected adapter to the Degraded intermediate (not Disconnected).
func TestFleetAdapter_MissedProbeGoesDegraded(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now.Add(-15 * time.Second)) // 1×interval ≤ 15s < 3×interval
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseConnected, &hb)
	if got := reconcilePhase(t, r, c); got != fleetv1.FleetAdapterPhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", got)
	}
}

func TestFleetAdapter_DegradedRecoversOnFreshHeartbeat(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now.Add(-3 * time.Second)) // fresh again
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseDegraded, &hb)
	if got := reconcilePhase(t, r, c); got != fleetv1.FleetAdapterPhaseConnected {
		t.Fatalf("phase = %s, want Connected (recovered)", got)
	}
}

func TestFleetAdapter_DegradedGoesDisconnectedWhenStale(t *testing.T) {
	now := time.Unix(1000, 0)
	hb := metav1.NewTime(now.Add(-40 * time.Second))
	r, c := newFAReconciler(t, now, fleetv1.FleetAdapterPhaseDegraded, &hb)
	if got := reconcilePhase(t, r, c); got != fleetv1.FleetAdapterPhaseDisconnected {
		t.Fatalf("phase = %s, want Disconnected", got)
	}
}
