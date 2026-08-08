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

package main

// ControlStream mTLS wiring (RFC-0001 §9.2.7, ADR-0025): the fail-closed decision
// and the server TLS-config builder.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSigned emits a matching cert/key PEM pair to dir and returns their
// paths. The certificate is also a usable CA PEM (self-signed) for the client-CA
// pool in the "all three set" case.
func writeSelfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "controlstream-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"swarmada-controlstream.system.svc"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func TestControlStreamShouldServe(t *testing.T) {
	cfg := &tls.Config{}
	cases := []struct {
		name     string
		tlsCfg   *tls.Config
		insecure bool
		want     bool
	}{
		{"no tls, secure -> fail closed (not served)", nil, false, false},
		{"no tls, insecure -> plaintext served (dev)", nil, true, true},
		{"tls, secure -> served", cfg, false, true},
		{"tls, insecure -> served", cfg, true, true},
	}
	for _, tc := range cases {
		if got := controlStreamShouldServe(tc.tlsCfg, tc.insecure); got != tc.want {
			t.Errorf("%s: controlStreamShouldServe = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildControlStreamTLS_NoneSet_ReturnsNilNoError(t *testing.T) {
	cfg, err := buildControlStreamTLS("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config when no TLS files are set, got %+v", cfg)
	}
}

func TestBuildControlStreamTLS_PartialIsError(t *testing.T) {
	cases := [][3]string{
		{"cert", "", ""},
		{"", "key", ""},
		{"", "", "ca"},
		{"cert", "key", ""},
		{"cert", "", "ca"},
		{"", "key", "ca"},
	}
	for _, c := range cases {
		cfg, err := buildControlStreamTLS(c[0], c[1], c[2])
		if err == nil {
			t.Errorf("buildControlStreamTLS(%q,%q,%q): expected an all-or-none error", c[0], c[1], c[2])
		}
		if cfg != nil {
			t.Errorf("buildControlStreamTLS(%q,%q,%q): config must be nil on error", c[0], c[1], c[2])
		}
	}
}

func TestBuildControlStreamTLS_ValidPolicy(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)
	// the self-signed cert doubles as the client-CA PEM.
	cfg, err := buildControlStreamTLS(certFile, keyFile, certFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected a non-nil config when all three files are set")
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert (fail closed)", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3 (%x)", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs pool must be populated")
	}
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate must be set (hot reload)")
	}
	if _, err := cfg.GetCertificate(nil); err != nil {
		t.Errorf("GetCertificate must serve the keypair: %v", err)
	}
}

func TestBuildControlStreamTLS_EmptyCAIsError(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)
	badCA := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := buildControlStreamTLS(certFile, keyFile, badCA)
	if err == nil {
		t.Fatal("expected an error for a client-CA file with no valid certificates")
	}
	if cfg != nil {
		t.Error("config must be nil on error")
	}
}
