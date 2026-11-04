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

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestServerTLSConfigRequiresClientCert(t *testing.T) {
	p := newPKI(t)
	cfg, err := serverTLSConfig(p.serverCert, p.serverKey, p.caFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert (mTLS)", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil; adapter client certs would not be verified")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS1.3", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("server presents %d certificates, want 1", len(cfg.Certificates))
	}
}

func TestServerTLSConfigBadCert(t *testing.T) {
	p := newPKI(t)
	if _, err := serverTLSConfig(p.caFile, p.serverKey, p.caFile); err == nil {
		t.Fatal("expected error when cert/key do not match")
	}
	if _, err := serverTLSConfig(filepath.Join(t.TempDir(), "nope.crt"), p.serverKey, p.caFile); err == nil {
		t.Fatal("expected error for a missing cert file")
	}
}

func TestServerTLSConfigBadCA(t *testing.T) {
	p := newPKI(t)
	empty := filepath.Join(t.TempDir(), "empty-ca.crt")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serverTLSConfig(p.serverCert, p.serverKey, empty); err == nil {
		t.Fatal("expected error for a CA bundle with no valid certificates")
	}
	if _, err := serverTLSConfig(p.serverCert, p.serverKey, filepath.Join(t.TempDir(), "nope.crt")); err == nil {
		t.Fatal("expected error for a missing CA file")
	}
}
