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
	"crypto/x509"
	"fmt"
	"os"
)

// serverTLSConfig builds the edge node's mTLS server configuration (RFC-0001
// #fleet-adapter-protocol-transport-security, #security-transport-security). It presents the server certificate from certFile/keyFile and
// REQUIRES every client (Fleet Adapter) to present a certificate that verifies
// against the namespace CA bundle in clientCAFile — the same transport-security
// model the control plane's ControlStream uses. certFile/keyFile/clientCAFile are
// the tls.crt/tls.key/ca.crt keys of the Secret named in
// FleetZone.spec.edgeNode.tlsSecretRef, projected onto the edge host as files.
//
// A client that presents no certificate, or one not issued by the namespace CA, is
// rejected at the TLS handshake — it never reaches EdgeStream.
func serverTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load edge server keypair (%q, %q): %w", certFile, keyFile, err)
	}

	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read namespace CA bundle %q: %w", clientCAFile, err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("namespace CA bundle %q contains no valid certificates", clientCAFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert, // mTLS: no anonymous adapters
		MinVersion:   tls.VersionTLS13,
	}, nil
}
