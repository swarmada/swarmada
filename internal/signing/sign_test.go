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
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// The producer and the verifier must agree, or a signed conformance report is
// indistinguishable from a forged one: both fail. This is the round trip the control
// plane actually performs — Sign, EncodeInline, ParseInlineSignature, Verify.
func TestSignVerifyRoundTrip(t *testing.T) {
	report := []byte(`{"adapter":"simulation","conformant":true,"contract_version":"1.0.0"}`)

	for _, tc := range []struct {
		name string
		gen  func() (crypto.Signer, any)
	}{
		{"ed25519", func() (crypto.Signer, any) {
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			return priv, pub
		}},
		{"ecdsa-p256", func() (crypto.Signer, any) {
			k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			return k, k.Public()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			priv, pub := tc.gen()
			der, err := x509.MarshalPKIXPublicKey(pub)
			if err != nil {
				t.Fatalf("marshal pub: %v", err)
			}
			root, err := ParseTrustRoot("vendor", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
			if err != nil {
				t.Fatalf("ParseTrustRoot: %v", err)
			}
			sig, err := Sign(report, priv)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			raw, ok := ParseInlineSignature(EncodeInline(sig))
			if !ok {
				t.Fatal("EncodeInline output is unreadable by ParseInlineSignature")
			}
			if _, err := Verify(report, raw, []TrustRoot{root}); err != nil {
				t.Errorf("Verify rejected a signature this package produced: %v", err)
			}
		})
	}
}

// Signing a report and verifying it against the matching trust root must succeed, and
// verifying a TAMPERED report against the same signature must fail — a signature that
// validates altered bytes is worse than no signature at all.
func TestSignedReportVerifiesAndTamperFails(t *testing.T) {
	report := []byte(`{"adapter":"simulation","conformant":true}`)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	root, err := ParseTrustRoot("vendor-a", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err != nil {
		t.Fatalf("ParseTrustRoot: %v", err)
	}

	sig, err := Sign(report, priv)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, ok := ParseInlineSignature(EncodeInline(sig))
	if !ok {
		t.Fatal("EncodeInline produced something ParseInlineSignature cannot read")
	}
	signer, err := Verify(report, raw, []TrustRoot{root})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if signer != "vendor-a" {
		t.Errorf("signer = %q, want vendor-a", signer)
	}

	tampered := []byte(`{"adapter":"simulation","conformant":false}`)
	if _, err := Verify(tampered, raw, []TrustRoot{root}); err == nil {
		t.Error("a tampered report verified against the original signature")
	}
}

// A PKCS#8 ed25519 key round-trips through ParseSigningKey, which is how the CLI
// loads an operator's key from disk.
func TestParseSigningKeyPKCS8(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signer, err := ParseSigningKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatalf("ParseSigningKey: %v", err)
	}
	if _, err := Sign([]byte("x"), signer); err != nil {
		t.Errorf("parsed key cannot sign: %v", err)
	}
	if _, err := ParseSigningKey([]byte("not pem")); err == nil {
		t.Error("garbage PEM must be refused")
	}
}
