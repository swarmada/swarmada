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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/registry"
)

// registryCredsSecret builds the conventional per-namespace credentials Secret.
func registryCredsSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryCredentialsSecret, Namespace: rolloutNS},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

// fakeOCIFetcher returns preset blob bytes (keyed loosely) and records the call.
type fakeOCIFetcher struct {
	blob      []byte
	err       error
	layers    []registry.Layer // for tag-addressed resolution
	layersErr error
	gotDigest string
	gotRepo   string
	gotReg    string
	gotRef    string // manifest ref (tag) passed to Layers
	gotCred   *registry.Credential
	callCount int
}

func (f *fakeOCIFetcher) Blob(_ context.Context, reg, repo, digest string, cred *registry.Credential) ([]byte, error) {
	f.callCount++
	f.gotReg, f.gotRepo, f.gotDigest, f.gotCred = reg, repo, digest, cred
	return f.blob, f.err
}

func (f *fakeOCIFetcher) Layers(_ context.Context, reg, repo, ref string, cred *registry.Credential) ([]registry.Layer, error) {
	f.gotReg, f.gotRepo, f.gotRef, f.gotCred = reg, repo, ref, cred
	return f.layers, f.layersErr
}

func ociRef(sig []byte) string {
	sum := sha256.Sum256(sig)
	return "oci://reg.example:5000/firmware/acme@sha256:" + hex.EncodeToString(sum[:])
}

// A digest-addressed oci:// signature is fetched, content-verified, and dispatched.
func TestFirmwareRollout_OCIFetchVerifiesAndDispatches(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(ociRef(rawSig)),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	oci := &fakeOCIFetcher{blob: rawSig}
	r.OCIFetcher = oci

	reconcileFirmware(t, r)

	if oci.callCount != 1 || oci.gotReg != "reg.example:5000" || oci.gotRepo != "firmware/acme" {
		t.Fatalf("oci fetch = reg=%q repo=%q calls=%d", oci.gotReg, oci.gotRepo, oci.callCount)
	}
	if !robotPending(t, c, "r1") {
		t.Fatal("oci-fetched verified rollout should have annotated the eligible robot")
	}
}

// The conventional registry credential is passed through to the OCI fetch.
func TestFirmwareRollout_OCIFetchUsesCredentials(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	creds := registryCredsSecret("registry-tok")
	r, _ := newFirmwareReconciler(t, fwRollout(ociRef(rawSig)),
		signingConfig(true), secret, creds, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	oci := &fakeOCIFetcher{blob: rawSig}
	r.OCIFetcher = oci

	reconcileFirmware(t, r)

	if oci.gotCred == nil || oci.gotCred.BearerToken != "registry-tok" {
		t.Fatalf("oci credential = %+v, want bearer registry-tok", oci.gotCred)
	}
}

// A registry that serves bytes NOT matching the requested digest fails closed
// (content-address integrity) — never dispatched, even before signature checks.
func TestFirmwareRollout_OCIDigestMismatchFailsClosed(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(ociRef(rawSig)),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.OCIFetcher = &fakeOCIFetcher{blob: []byte("tampered-bytes-not-matching-digest")}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("a digest-mismatched oci blob must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Error("digest mismatch should fail the rollout closed")
	}
}

func layerDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// A tag-addressed oci:// ref resolves the manifest's single signature layer,
// content-verifies it, and dispatches.
func TestFirmwareRollout_OCITagVerifiesAndDispatches(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("oci://reg.example:5000/firmware/acme:2.5.0-sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	oci := &fakeOCIFetcher{blob: rawSig, layers: []registry.Layer{{Digest: layerDigest(rawSig)}}}
	r.OCIFetcher = oci

	reconcileFirmware(t, r)

	if oci.gotRef != "2.5.0-sig" || oci.gotRepo != "firmware/acme" {
		t.Fatalf("Layers resolved ref=%q repo=%q", oci.gotRef, oci.gotRepo)
	}
	if oci.gotDigest != layerDigest(rawSig) {
		t.Fatalf("Blob fetched digest=%q, want the manifest layer digest", oci.gotDigest)
	}
	if !robotPending(t, c, "r1") {
		t.Fatal("tag-addressed oci signature should verify and dispatch")
	}
}

// A tag whose manifest has more than one layer is ambiguous → fail closed.
func TestFirmwareRollout_OCITagMultiLayerFailsClosed(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("oci://reg.example:5000/firmware/acme:sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.OCIFetcher = &fakeOCIFetcher{blob: rawSig, layers: []registry.Layer{{Digest: "sha256:a"}, {Digest: "sha256:b"}}}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("a multi-layer signature manifest must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Error("multi-layer should fail closed")
	}
}

// A layer blob that does not match the manifest's declared layer digest fails
// closed (content-address integrity) before any signature check.
func TestFirmwareRollout_OCITagLayerDigestMismatchFailsClosed(t *testing.T) {
	secret, rawSig := rawSigningFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout("oci://reg.example:5000/firmware/acme:sig"),
		signingConfig(true), secret, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	// Manifest claims digest X, but the blob is different bytes.
	r.OCIFetcher = &fakeOCIFetcher{blob: []byte("tampered"), layers: []registry.Layer{{Digest: layerDigest(rawSig)}}}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("a layer-digest-mismatched blob must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Error("layer digest mismatch should fail closed")
	}
}

func TestParseOCIRef(t *testing.T) {
	dig := "sha256:" + hex.EncodeToString(make([]byte, 32))

	// Digest form.
	reg, repo, ref, isDigest, err := parseOCIRef("oci://ghcr.io/swarmada/fw@" + dig)
	if err != nil || reg != "ghcr.io" || repo != "swarmada/fw" || ref != dig || !isDigest {
		t.Fatalf("digest parse = %q/%q/%q isDigest=%v err=%v", reg, repo, ref, isDigest, err)
	}
	// Tag form, with a registry PORT (the port colon must not be mistaken for a tag).
	reg, repo, ref, isDigest, err = parseOCIRef("oci://reg.example:5000/firmware/acme:2.5.0-sig")
	if err != nil || reg != "reg.example:5000" || repo != "firmware/acme" || ref != "2.5.0-sig" || isDigest {
		t.Fatalf("tag parse = %q/%q/%q isDigest=%v err=%v", reg, repo, ref, isDigest, err)
	}

	for _, bad := range []string{
		"oci://ghcr.io@" + dig,          // digest form, no repo path
		"oci://ghcr.io/fw@sha256:short", // malformed digest
		"oci://ghcr.io/fw@md5:x",        // wrong algo
		"oci://ghcr.io/justrepo",        // tag form, no tag
		"oci://ghcr.io",                 // no repo at all
	} {
		if _, _, _, _, err := parseOCIRef(bad); err == nil {
			t.Errorf("parseOCIRef(%q) should have errored", bad)
		}
	}
}
