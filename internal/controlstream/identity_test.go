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

package controlstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestParseAdapterSAN(t *testing.T) {
	cases := []struct {
		san         string
		wantAdapter string
		wantNS      string
		wantOK      bool
	}{
		{"acme-adapter.warehouse-a.svc.cluster.local", "acme-adapter", "warehouse-a", true},
		{"acme-adapter.warehouse-a.svc.cluster.local.evil.com", "", "", false},
		{"acme-adapter.warehouse-a.pod.cluster.local", "", "", false},
		{".warehouse-a.svc.cluster.local", "", "", false},
		{"acme-adapter..svc.cluster.local", "", "", false},
		{"acme-adapter.warehouse-a.svc.cluster", "", "", false},
		{"plain-hostname", "", "", false},
	}
	for _, tc := range cases {
		id, ok := parseAdapterSAN(tc.san)
		if ok != tc.wantOK || id.AdapterName != tc.wantAdapter || id.Namespace != tc.wantNS {
			t.Errorf("parseAdapterSAN(%q) = %+v,%v; want %s/%s,%v", tc.san, id, ok, tc.wantAdapter, tc.wantNS, tc.wantOK)
		}
	}
}

// A verified client cert with a well-formed SAN yields a verified identity.
func TestIdentityFromContext_VerifiedCert(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"acme-adapter.warehouse-a.svc.cluster.local"}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{cert}},
		}},
	})
	id := IdentityFromContext(ctx)
	if !id.Verified || id.AdapterName != "acme-adapter" || id.Namespace != "warehouse-a" {
		t.Fatalf("IdentityFromContext = %+v", id)
	}
}

// No peer / no TLS / no verified chain all yield an UNVERIFIED identity (the
// enforcement layer then fails closed).
func TestIdentityFromContext_Unverified(t *testing.T) {
	if id := IdentityFromContext(context.Background()); id.Verified {
		t.Error("no peer must be unverified")
	}
	// TLS present but the chain was not verified (PeerCertificates set, VerifiedChains empty).
	cert := &x509.Certificate{DNSNames: []string{"acme-adapter.warehouse-a.svc.cluster.local"}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{cert}, // NOT verified
		}},
	})
	if id := IdentityFromContext(ctx); id.Verified {
		t.Error("an unverified chain must not produce a verified identity")
	}
}
