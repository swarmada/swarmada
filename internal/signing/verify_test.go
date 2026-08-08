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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func pkixPEM(t *testing.T, pub interface{}) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

const goodChecksum = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// A genuine ed25519 signature over the checksum verifies and returns the signer.
func TestVerify_Ed25519_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root, err := ParseTrustRoot("ci-signer", pkixPEM(t, pub))
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(goodChecksum))

	signer, err := Verify([]byte(goodChecksum), sig, []TrustRoot{root})
	if err != nil || signer != "ci-signer" {
		t.Fatalf("Verify = %q, %v; want ci-signer, nil", signer, err)
	}
}

// A tampered checksum (artifact swapped after signing) MUST fail verification.
func TestVerify_TamperedChecksumRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root, _ := ParseTrustRoot("ci-signer", pkixPEM(t, pub))
	sig := ed25519.Sign(priv, []byte(goodChecksum))

	tampered := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := Verify([]byte(tampered), sig, []TrustRoot{root}); err == nil {
		t.Fatal("tampered checksum verified — MUST fail closed")
	}
}

// A signature from an untrusted key MUST NOT verify against the configured root.
func TestVerify_WrongKeyRejected(t *testing.T) {
	trustedPub, _, _ := ed25519.GenerateKey(rand.Reader)
	root, _ := ParseTrustRoot("ci-signer", pkixPEM(t, trustedPub))

	_, attackerPriv, _ := ed25519.GenerateKey(rand.Reader) // not a trust root
	sig := ed25519.Sign(attackerPriv, []byte(goodChecksum))

	if _, err := Verify([]byte(goodChecksum), sig, []TrustRoot{root}); err == nil {
		t.Fatal("signature from an untrusted key verified — MUST fail closed")
	}
}

// ECDSA-P256 signatures are supported.
func TestVerify_ECDSA_Valid(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	root, err := ParseTrustRoot("ecdsa-signer", pkixPEM(t, &priv.PublicKey))
	if err != nil {
		t.Fatalf("parse root: %v", err)
	}
	digest := sha256.Sum256([]byte(goodChecksum))
	sig, _ := ecdsa.SignASN1(rand.Reader, priv, digest[:])

	signer, err := Verify([]byte(goodChecksum), sig, []TrustRoot{root})
	if err != nil || signer != "ecdsa-signer" {
		t.Fatalf("Verify = %q, %v; want ecdsa-signer, nil", signer, err)
	}
}

// Empty signature / no roots fail closed.
func TestVerify_FailsClosedOnMissingInputs(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root, _ := ParseTrustRoot("s", pkixPEM(t, pub))
	sig := ed25519.Sign(priv, []byte(goodChecksum))

	if _, err := Verify([]byte(goodChecksum), nil, []TrustRoot{root}); err == nil {
		t.Error("empty signature must fail")
	}
	if _, err := Verify([]byte(goodChecksum), sig, nil); err == nil {
		t.Error("no trust roots must fail")
	}
}

func TestParseTrustRoot_RejectsGarbage(t *testing.T) {
	if _, err := ParseTrustRoot("x", []byte("not a pem")); err == nil {
		t.Fatal("garbage PEM must be rejected")
	}
}

func TestParseInlineSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	root, _ := ParseTrustRoot("s", pkixPEM(t, pub))
	sig := ed25519.Sign(priv, []byte(goodChecksum))
	ref := "bundle:" + b64(sig)

	got, ok := ParseInlineSignature(ref)
	if !ok {
		t.Fatal("inline bundle not recognised")
	}
	if _, err := Verify([]byte(goodChecksum), got, []TrustRoot{root}); err != nil {
		t.Fatalf("decoded inline signature failed to verify: %v", err)
	}
	if _, ok := ParseInlineSignature("oci://registry/x:1.0.sig"); ok {
		t.Fatal("non-inline ref must not be treated as inline")
	}
}

func TestValidChecksum(t *testing.T) {
	if !ValidChecksum(goodChecksum) {
		t.Error("well-formed checksum rejected")
	}
	for _, bad := range []string{"", "sha256:xyz", "e3b0c442...", "sha256:" + "zz", "md5:abcd"} {
		if ValidChecksum(bad) {
			t.Errorf("malformed checksum %q accepted", bad)
		}
	}
}
