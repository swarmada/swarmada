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

// Package signing verifies firmware and model artifact signatures against the
// trust roots configured in SwarmadaConfig.spec.signing (RFC-0001 §9.1.7, §9.2.8).
// Verification fails closed: any missing input, unparseable key, or non-matching
// signature returns an error, and the caller MUST refuse to dispatch the artifact.
package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// bundleScheme marks an inline, base64-encoded detached signature carried in the
// signatureRef itself (no network fetch required).
const bundleScheme = "bundle:"

// TrustRoot is a parsed public-key anchor. Name is the operator-facing identifier
// recorded as the signer identity in the audit log.
type TrustRoot struct {
	Name      string
	PublicKey crypto.PublicKey
}

// ParseTrustRoot parses PEM material (a PKIX public key or an x509 certificate)
// into a TrustRoot. Only ed25519 and ECDSA keys are accepted.
func ParseTrustRoot(name string, pemBytes []byte) (TrustRoot, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return TrustRoot{}, fmt.Errorf("trust root %q: no PEM block found", name)
	}

	var pub crypto.PublicKey
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		pub = k
	} else if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		pub = cert.PublicKey
	} else {
		return TrustRoot{}, fmt.Errorf("trust root %q: unsupported PEM (want PKIX public key or x509 certificate)", name)
	}

	switch pub.(type) {
	case ed25519.PublicKey, *ecdsa.PublicKey:
		return TrustRoot{Name: name, PublicKey: pub}, nil
	default:
		return TrustRoot{}, fmt.Errorf("trust root %q: unsupported key type %T (want ed25519 or ECDSA)", name, pub)
	}
}

// Verify reports the name of the first trust root whose public key validates the
// signature over payload. It fails closed: an empty root set, an empty signature,
// or no matching root all return an error and no signer identity.
func Verify(payload, signature []byte, roots []TrustRoot) (string, error) {
	if len(roots) == 0 {
		return "", errors.New("no trust roots configured; cannot verify signature")
	}
	if len(signature) == 0 {
		return "", errors.New("empty signature")
	}
	for _, root := range roots {
		if verifyOne(root.PublicKey, payload, signature) {
			return root.Name, nil
		}
	}
	return "", errors.New("signature does not verify against any configured trust root")
}

func verifyOne(pub crypto.PublicKey, payload, sig []byte) bool {
	switch k := pub.(type) {
	case ed25519.PublicKey:
		return ed25519.Verify(k, payload, sig)
	case *ecdsa.PublicKey:
		digest := sha256.Sum256(payload)
		return ecdsa.VerifyASN1(k, digest[:], sig)
	default:
		return false
	}
}

// ParseInlineSignature decodes an inline "bundle:<base64>" signatureRef into raw
// signature bytes. The second return is false for any other (fetchable) ref form,
// which the caller must obtain via a fetcher — and, absent one, treat as
// unverifiable (fail closed).
func ParseInlineSignature(ref string) ([]byte, bool) {
	if !strings.HasPrefix(ref, bundleScheme) {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ref, bundleScheme))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// ValidChecksum reports whether c is a well-formed "sha256:<64 hex>" digest.
func ValidChecksum(c string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(c, prefix) {
		return false
	}
	h := strings.TrimPrefix(c, prefix)
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}
