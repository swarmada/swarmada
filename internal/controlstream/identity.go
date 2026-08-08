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
	"strings"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// TLSIdentity is the authenticated identity of a connected adapter, derived from
// its mTLS client-certificate SAN (RFC-0001 §9.2.7). Unlike AdapterHello's
// self-reported fields, this IS the security boundary: every per-robot
// authorization decision is made against it. Verified is false when the
// connection presented no usable client certificate — in which case an enabled
// Authorizer denies every robot-scoped message (fail closed).
type TLSIdentity struct {
	// AdapterName is the FleetAdapter this transport identity binds to.
	AdapterName string
	// Namespace is the Swarmada namespace the SAN encodes.
	Namespace string
	// Verified is true only when a client certificate produced a well-formed SAN.
	Verified bool
}

// svcSANSuffix is the Kubernetes in-cluster service-DNS suffix that a
// well-formed adapter SAN ends with: <adapter>.<namespace>.svc.cluster.local.
var svcSANSuffix = []string{"svc", "cluster", "local"}

// IdentityFromContext extracts the adapter identity from the gRPC peer's verified
// TLS client certificate. It returns an unverified (zero-AdapterName) identity
// when there is no peer, no TLS auth info, no client certificate, or no SAN of the
// expected <adapter>.<namespace>.svc.cluster.local form — never an error, so the
// caller decides how strictly to enforce.
func IdentityFromContext(ctx context.Context) TLSIdentity {
	pr, ok := peer.FromContext(ctx)
	if !ok {
		return TLSIdentity{}
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return TLSIdentity{}
	}
	// VerifiedChains is populated only after the server successfully verified the
	// client certificate against the trust bundle; PeerCertificates alone is not
	// proof of verification. Require a verified chain.
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return TLSIdentity{}
	}
	leaf := tlsInfo.State.VerifiedChains[0][0]
	for _, san := range leaf.DNSNames {
		if id, ok := parseAdapterSAN(san); ok {
			return id
		}
	}
	return TLSIdentity{}
}

// parseAdapterSAN parses "<adapter>.<namespace>.svc.cluster.local" into an
// identity. The adapter and namespace labels must be non-empty and the suffix
// exact.
func parseAdapterSAN(san string) (TLSIdentity, bool) {
	parts := strings.Split(san, ".")
	if len(parts) != 2+len(svcSANSuffix) {
		return TLSIdentity{}, false
	}
	for i, want := range svcSANSuffix {
		if parts[2+i] != want {
			return TLSIdentity{}, false
		}
	}
	adapter, namespace := parts[0], parts[1]
	if adapter == "" || namespace == "" {
		return TLSIdentity{}, false
	}
	return TLSIdentity{AdapterName: adapter, Namespace: namespace, Verified: true}, true
}
