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

package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// ParseSigningKey parses PEM private-key material (PKCS#8 or SEC1) into a signer.
// Only ed25519 and ECDSA are accepted — the same two algorithms ParseTrustRoot
// accepts, so a key that can sign always has a verifiable counterpart.
func ParseSigningKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("signing key: no PEM block found")
	}
	var key any
	var err error
	if key, err = x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
			return nil, fmt.Errorf("signing key: unsupported PEM (want PKCS#8 or SEC1 EC private key)")
		}
	}
	switch k := key.(type) {
	case ed25519.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return k, nil
	default:
		return nil, fmt.Errorf("signing key: unsupported key type %T (want ed25519 or ECDSA)", key)
	}
}

// Sign produces a detached signature over payload.
//
// ed25519 signs the payload directly (it hashes internally, and crypto.Hash(0) is how
// its Signer is invoked); ECDSA signs a SHA-256 digest. This mirrors Verify, which
// tries the payload as-is for ed25519 and the digest for ECDSA — the two must agree or
// nothing this produces would validate.
func Sign(payload []byte, signer crypto.Signer) ([]byte, error) {
	switch k := signer.(type) {
	case ed25519.PrivateKey:
		return ed25519.Sign(k, payload), nil
	case *ecdsa.PrivateKey:
		return ecdsaSign(payload, k)
	default:
		return nil, fmt.Errorf("unsupported signer type %T", signer)
	}
}

// EncodeInline renders a detached signature as the inline "bundle:<base64>" form the
// conformance report and the rollout artifact fields carry, and that
// ParseInlineSignature reads back.
func EncodeInline(sig []byte) string {
	return bundleScheme + base64.StdEncoding.EncodeToString(sig)
}

func ecdsaSign(payload []byte, k *ecdsa.PrivateKey) ([]byte, error) {
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, k, digest[:])
}
