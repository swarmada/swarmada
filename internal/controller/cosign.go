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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/swarmada/swarmada/internal/registry"
)

// Cosign signature support (RFC-0001 §9.1.7.3).
//
// Swarmada's NATIVE signature form is a detached signature over the artifact checksum string: the
// blob IS the signature, and signing.Verify runs it against []byte(checksum).
//
// Cosign's form differs in both halves, which is why supporting it is more than relaxing a layer
// count:
//
//   - the signature is NOT the blob. The blob is a "simple signing" payload — a JSON document
//     describing what was signed — and the signature sits in the layer's
//     `dev.cosignproject.cosign/signature` annotation, base64-encoded.
//   - the signature is over that PAYLOAD, not over the bare checksum.
//
// So a cosign signature verified against []byte(checksum) always fails. Accepting the layout while
// still verifying the old way would turn today's clean, actionable rejection into a confusing
// verification failure later in the pipeline — worse than not supporting it.
//
// The security-critical step is BINDING: a valid cosign signature proves someone signed *a*
// payload, not that the payload describes *this* artifact. cosignPayloadDigest extracts the digest
// the payload actually attests, and the caller refuses when it does not match the artifact being
// dispatched. Without that check, a genuine signature for a different image would pass.

const (
	// cosignSignatureAnnotation holds the base64 signature on the signature layer.
	cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"
	// cosignPayloadMediaType is the media type of the simple-signing payload blob.
	cosignPayloadMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
)

// cosignLayer is the signature layer selected from a multi-layer artifact manifest.
type cosignLayer struct {
	// Digest addresses the simplesigning PAYLOAD blob (content-verified before use).
	Digest string
	// Signature is the raw signature bytes decoded from the layer annotation.
	Signature []byte
}

// selectCosignLayer finds the signature layer in a resolved manifest.
//
// Returns ok=false when the manifest carries no cosign layer, so the caller can fall back to the
// native single-layer form rather than treating every multi-layer artifact as cosign.
//
// A manifest carrying MORE than one cosign layer is an error, not a pick-the-first: cosign attaches
// one signature per signer, and silently choosing among them would let an artifact be admitted on a
// signature the operator never intended to trust.
func selectCosignLayer(layers []registry.Layer) (cosignLayer, bool, error) {
	var found []registry.Layer
	for _, l := range layers {
		if _, has := l.Annotations[cosignSignatureAnnotation]; has || l.MediaType == cosignPayloadMediaType {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 0:
		return cosignLayer{}, false, nil
	case 1:
	default:
		return cosignLayer{}, false, fmt.Errorf(
			"oci signature manifest carries %d cosign signature layers; refusing to choose between "+
				"signers — reference the intended signature by digest", len(found))
	}

	l := found[0]
	b64, has := l.Annotations[cosignSignatureAnnotation]
	if !has || b64 == "" {
		return cosignLayer{}, false, fmt.Errorf(
			"cosign layer %s carries no %s annotation (the signature lives in the annotation, not the blob)",
			l.Digest, cosignSignatureAnnotation)
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return cosignLayer{}, false, fmt.Errorf("cosign signature annotation is not valid base64: %w", err)
	}
	if l.Digest == "" {
		return cosignLayer{}, false, fmt.Errorf("cosign layer carries no digest for its payload blob")
	}
	return cosignLayer{Digest: l.Digest, Signature: sig}, true, nil
}

// simpleSigningPayload is the subset of the cosign/Red Hat simple-signing document this reads.
type simpleSigningPayload struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// cosignPayloadDigest returns the artifact digest a simplesigning payload attests.
//
// This is the binding step. A signature that verifies proves only that the payload is authentic;
// it says nothing about WHICH artifact the payload describes. The caller compares this against the
// checksum being dispatched and refuses on mismatch — otherwise a genuine, correctly-signed payload
// for some other image would satisfy the gate.
func cosignPayloadDigest(payload []byte) (string, error) {
	var p simpleSigningPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", fmt.Errorf("cosign payload is not valid simple-signing JSON: %w", err)
	}
	digest := strings.TrimSpace(p.Critical.Image.DockerManifestDigest)
	if digest == "" {
		return "", fmt.Errorf("cosign payload declares no critical.image.docker-manifest-digest")
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", fmt.Errorf("cosign payload digest %q is not a sha256 digest", digest)
	}
	return digest, nil
}
