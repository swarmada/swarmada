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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const fwChecksum = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signingFixture returns a trust-root Secret and an inline signature over payload.
func signingFixture(t *testing.T, payload string) (*corev1.Secret, string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "trust-secret", Namespace: rolloutNS},
		Data:       map[string][]byte{"cosign.pub": pubPEM},
	}
	sig := ed25519.Sign(priv, []byte(payload))
	return secret, "bundle:" + base64.StdEncoding.EncodeToString(sig)
}

func signingConfig(require bool) *fleetv1.SwarmadaConfig {
	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: rolloutNS},
		Spec: fleetv1.SwarmadaConfigSpec{
			Signing: fleetv1.SwarmadaSigningConfig{RequireSignatureVerification: require},
		},
	}
	if require {
		cfg.Spec.Signing.TrustRoots = []fleetv1.SigningTrustRoot{{
			Name:      "ci-signer",
			SecretRef: fleetv1.SigningSecretKeyRef{Name: "trust-secret", Key: "cosign.pub"},
		}}
	}
	return cfg
}

func fwRollout(sigRef string) *fleetv1.FirmwareRollout {
	return &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw", Namespace: rolloutNS},
		Spec: fleetv1.FirmwareRolloutSpec{
			TargetSelector:       metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			NewVersion:           "2.5.0",
			FirmwareURI:          "oci://reg/acme:2.5.0",
			FirmwareChecksum:     fwChecksum,
			FirmwareSignatureRef: sigRef,
			Strategy:             fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{MaxUnavailable: "1"}},
			SafetyConstraints:    fleetv1.RolloutSafetyConstraints{MinBatteryPct: 30, RequireIdleState: true},
		},
	}
}

func newFirmwareReconciler(t *testing.T, objs ...client.Object) (*FirmwareRolloutReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FirmwareRollout{}, &fleetv1.Robot{}).Build()
	return &FirmwareRolloutReconciler{Client: c, Scheme: scheme}, c
}

func reconcileFirmware(t *testing.T, r *FirmwareRolloutReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "fw", Namespace: rolloutNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getFirmwareRollout(t *testing.T, c client.Client) *fleetv1.FirmwareRollout {
	t.Helper()
	fw := &fleetv1.FirmwareRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fw", Namespace: rolloutNS}, fw); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	return fw
}

func robotPending(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	return getRobot(t, c, name, rolloutNS).Annotations[annPendingFirmwareVersion] != ""
}

// A valid signature verifies and dispatches the firmware to an eligible robot.
func TestFirmwareRollout_VerifiedDispatches(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if !robotPending(t, c, "r1") {
		t.Fatal("verified rollout should have annotated the eligible robot")
	}
	fw := getFirmwareRollout(t, c)
	if !conditionIsTrue(fw.Status.Conditions, conditionSignatureVerified) {
		t.Fatalf("SignatureVerified condition not True: %+v", fw.Status.Conditions)
	}
}

// A TAMPERED checksum (signature no longer matches) fails closed: rollout Failed,
// SignatureVerified=False, and NO robot is annotated.
func TestFirmwareRollout_TamperedChecksumFailsClosed(t *testing.T) {
	// Sign the ORIGINAL checksum, then tamper the rollout's checksum.
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	ro.Spec.FirmwareChecksum = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("SECURITY: a tampered artifact was dispatched to a robot")
	}
	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhaseFailed || !conditionIsFalse(fw.Status.Conditions, conditionSignatureVerified) {
		t.Fatalf("expected Failed + SignatureVerified=False, got phase=%s conds=%+v", fw.Status.Phase, fw.Status.Conditions)
	}
}

// An UNSIGNED artifact (no signatureRef) with signing enforced fails closed.
func TestFirmwareRollout_UnsignedFailsClosed(t *testing.T) {
	secret, _ := signingFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(""), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("SECURITY: an unsigned artifact was dispatched")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Fatal("unsigned artifact must fail the rollout closed")
	}
}

// A signature from an untrusted key does not verify → fail closed.
func TestFirmwareRollout_WrongSignerFailsClosed(t *testing.T) {
	trustSecret, _ := signingFixture(t, fwChecksum) // trust root A
	_, attackerSig := signingFixture(t, fwChecksum) // signature from key B
	r, c := newFirmwareReconciler(t, fwRollout(attackerSig), signingConfig(true), trustSecret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("SECURITY: artifact signed by an untrusted key was dispatched")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Fatal("untrusted signer must fail the rollout closed")
	}
}

// With verification disabled, dispatch proceeds (checksum format still enforced).
func TestFirmwareRollout_VerificationDisabledDispatches(t *testing.T) {
	r, c := newFirmwareReconciler(t, fwRollout(""), signingConfig(false),
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if !robotPending(t, c, "r1") {
		t.Fatal("with signing disabled, the rollout should dispatch")
	}
}

// A malformed checksum is rejected even when signing is disabled.
func TestFirmwareRollout_MalformedChecksumFailsClosed(t *testing.T) {
	ro := fwRollout("")
	ro.Spec.FirmwareChecksum = "not-a-digest"
	r, c := newFirmwareReconciler(t, ro, signingConfig(false),
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("malformed checksum must not dispatch")
	}
}
