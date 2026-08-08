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
	"encoding/base64"
	"strings"
	"testing"

	"github.com/swarmada/swarmada/internal/registry"
)

// Cosign support (§9.1.7.3). The layout half is easy; the half that matters is BINDING — proving
// the signed payload describes the artifact actually being dispatched.
//
// A cosign signature that verifies proves someone signed *a* payload. Nothing about that says the
// payload describes THIS firmware. cosignPayloadDigest exists so the caller can refuse a genuine,
// correctly-signed payload that attests a different image — an attack that costs nothing to mount
// if the binding is skipped (reuse any signature the attacker can read off a public registry).

const testDigest = "sha256:" + "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

func cosignAnnotatedLayer(digest, sigB64 string) registry.Layer {
	return registry.Layer{
		Digest:      digest,
		MediaType:   cosignPayloadMediaType,
		Annotations: map[string]string{cosignSignatureAnnotation: sigB64},
	}
}

func TestSelectCosignLayer_FindsAnnotatedLayer(t *testing.T) {
	sig := []byte("raw-signature-bytes")
	layers := []registry.Layer{
		{Digest: "sha256:aa", MediaType: "application/vnd.oci.image.layer.v1.tar"},
		cosignAnnotatedLayer(testDigest, base64.StdEncoding.EncodeToString(sig)),
	}
	cl, ok, err := selectCosignLayer(layers)
	if err != nil || !ok {
		t.Fatalf("expected a cosign layer, got ok=%v err=%v", ok, err)
	}
	if cl.Digest != testDigest {
		t.Errorf("payload digest = %q, want %q", cl.Digest, testDigest)
	}
	if string(cl.Signature) != string(sig) {
		t.Errorf("signature = %q, want the decoded annotation %q", cl.Signature, sig)
	}
}

// A manifest with no cosign layer reports ok=false rather than erroring, so the caller can still
// use the native single-layer form.
func TestSelectCosignLayer_NonCosignManifest(t *testing.T) {
	layers := []registry.Layer{{Digest: "sha256:aa", MediaType: "application/octet-stream"}}
	cl, ok, err := selectCosignLayer(layers)
	if err != nil {
		t.Fatalf("a plain manifest must not error: %v", err)
	}
	if ok {
		t.Errorf("reported a cosign layer for a plain manifest: %+v", cl)
	}
}

// Multiple signers must NOT be silently resolved by picking one — the operator would have no idea
// which signature admitted the artifact.
func TestSelectCosignLayer_MultipleSignersRefused(t *testing.T) {
	sig := base64.StdEncoding.EncodeToString([]byte("s"))
	layers := []registry.Layer{
		cosignAnnotatedLayer("sha256:aa", sig),
		cosignAnnotatedLayer("sha256:bb", sig),
	}
	if _, _, err := selectCosignLayer(layers); err == nil {
		t.Fatal("two cosign signature layers must be refused, not silently narrowed to one")
	}
}

// A cosign-typed layer with no signature annotation is malformed, not "no signature".
func TestSelectCosignLayer_MissingAnnotationRefused(t *testing.T) {
	layers := []registry.Layer{{Digest: testDigest, MediaType: cosignPayloadMediaType}}
	_, _, err := selectCosignLayer(layers)
	if err == nil {
		t.Fatal("a cosign payload layer with no signature annotation must be refused")
	}
	if !strings.Contains(err.Error(), cosignSignatureAnnotation) {
		t.Errorf("error = %q, want it to name the missing annotation", err.Error())
	}
}

func TestSelectCosignLayer_BadBase64Refused(t *testing.T) {
	layers := []registry.Layer{cosignAnnotatedLayer(testDigest, "!!!not-base64!!!")}
	if _, _, err := selectCosignLayer(layers); err == nil {
		t.Fatal("a non-base64 signature annotation must be refused")
	}
}

// ── the binding half ──────────────────────────────────────────────────────────

func TestCosignPayloadDigest_ExtractsAttestedDigest(t *testing.T) {
	payload := []byte(`{"critical":{"identity":{"docker-reference":"example/fw"},` +
		`"image":{"docker-manifest-digest":"` + testDigest + `"},"type":"cosign container image signature"}}`)
	got, err := cosignPayloadDigest(payload)
	if err != nil {
		t.Fatalf("a well-formed payload must parse: %v", err)
	}
	if got != testDigest {
		t.Errorf("attested digest = %q, want %q", got, testDigest)
	}
}

// The attack this defends: a real payload, really signed — for something else.
func TestCosignPayloadDigest_DifferentArtifactIsVisible(t *testing.T) {
	other := "sha256:" + strings.Repeat("ab", 32)
	payload := []byte(`{"critical":{"image":{"docker-manifest-digest":"` + other + `"}}}`)
	got, err := cosignPayloadDigest(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got == testDigest {
		t.Fatal("payload for a different artifact must not report the dispatched digest")
	}
	if got != other {
		t.Errorf("attested digest = %q, want %q", got, other)
	}
}

func TestCosignPayloadDigest_RejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"not json", `not-json-at-all`},
		{"no digest", `{"critical":{"image":{}}}`},
		{"empty digest", `{"critical":{"image":{"docker-manifest-digest":"   "}}}`},
		{"not sha256", `{"critical":{"image":{"docker-manifest-digest":"md5:abc"}}}`},
		{"truncated digest", `{"critical":{"image":{"docker-manifest-digest":"sha256:abc"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cosignPayloadDigest([]byte(tc.payload)); err == nil {
				t.Errorf("payload %q must be refused", tc.payload)
			}
		})
	}
}
