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
	"encoding/pem"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/artifact"
)

// fakeFetcher stands in for the https fetcher: it returns a preset signature (or
// error) and records what it was asked to fetch, so a controller test needn't run
// a TLS server.
type fakeFetcher struct {
	sig     []byte
	err     error
	gotRef  string
	gotCred *artifact.Credential
	calls   int
}

func (f *fakeFetcher) Fetch(_ context.Context, ref string, cred *artifact.Credential) ([]byte, error) {
	f.calls++
	f.gotRef = ref
	f.gotCred = cred
	return f.sig, f.err
}

// rawSigningFixture returns a trust-root Secret and the RAW detached signature
// bytes over payload (what an https fetch would return, vs. the inline bundle).
func rawSigningFixture(t *testing.T, payload string) (*corev1.Secret, []byte) {
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
	return secret, ed25519.Sign(priv, []byte(payload))
}

// An https signatureRef is fetched, verified, and dispatched.
func TestFirmwareRollout_HTTPSFetchVerifiesAndDispatches(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/acme-2.5.0.sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	ff := &fakeFetcher{sig: rawSig}
	r.Fetcher = ff

	reconcileFirmware(t, r)

	if ff.calls != 1 || ff.gotRef != "https://artifacts.example.com/acme-2.5.0.sig" {
		t.Fatalf("fetcher called %d times, ref=%q", ff.calls, ff.gotRef)
	}
	if ff.gotCred != nil {
		t.Errorf("no credentials Secret present, want anonymous fetch; got %+v", ff.gotCred)
	}
	if !robotPending(t, c, "r1") {
		t.Fatal("https-fetched verified rollout should have annotated the eligible robot")
	}
}

// The conventional registry-credentials Secret is passed to the fetcher.
func TestFirmwareRollout_HTTPSFetchUsesRegistryCredentials(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryCredentialsSecret, Namespace: rolloutNS},
		Data:       map[string][]byte{"token": []byte("registry-tok")},
	}
	r, _ := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	ff := &fakeFetcher{sig: rawSig}
	r.Fetcher = ff

	reconcileFirmware(t, r)

	if ff.gotCred == nil || ff.gotCred.BearerToken != "registry-tok" {
		t.Fatalf("fetcher credential = %+v, want bearer registry-tok", ff.gotCred)
	}
}

// A tampered/mismatched fetched signature fails closed (never dispatched).
func TestFirmwareRollout_HTTPSFetchBadSignatureFailsClosed(t *testing.T) {
	secret, _ := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("https://artifacts.example.com/x.sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Fetcher = &fakeFetcher{sig: []byte("not-a-valid-signature")}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("a bad fetched signature must not dispatch")
	}
	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Errorf("phase = %s, want Failed on bad signature", fw.Status.Phase)
	}
}

// An oci:// ref is not yet wired: the default fetcher rejects the scheme and the
// rollout fails closed with an honest message (no fetcher injected).
func TestFirmwareRollout_OCIRefFailsClosed(t *testing.T) {
	secret, _ := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("oci://registry.example.com/acme:2.5.0.sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	// No r.Fetcher → DefaultFetcher, which rejects a non-https scheme with no network call.

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("an oci:// (unwired) ref must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Errorf("oci ref should fail closed to Failed")
	}
}
