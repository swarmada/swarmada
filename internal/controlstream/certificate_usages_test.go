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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// Every certificate the ControlStream client CA issues must say what it is for
// (RFC-0001 #fleet-adapter-protocol-transport-security).
//
// The ControlStream server certificate is issued by the same CA as every adapter client
// certificate, and its service SAN, swarmada-controlstream.system.svc.cluster.local, has
// the <adapter>.<namespace>.svc.cluster.local shape an adapter identity is parsed from.
// With no usages set, cert-manager requests digital signature and key encipherment and no
// extended key usage, and a leaf with no extended key usage is accepted by Go for any
// purpose, client authentication included. So the server's key, presented as a client,
// would be adapter swarmada-controlstream in namespace system. Setting server auth on it,
// and client auth on the client certificates, makes the handshake refuse that.

const serverServiceSAN = "swarmada-controlstream.system.svc.cluster.local"

// leafWithoutEKU signs a leaf the way cert-manager does when a Certificate sets no usages:
// digital signature and key encipherment, and no extended key usage extension.
func (ca *testCA) leafWithoutEKU(t *testing.T, sans []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The reason the manifests must set usages: without them the server certificate is an
// adapter identity.
func TestServerCertificateWithoutUsagesIsAcceptedAsAnAdapterIdentity(t *testing.T) {
	ca := newTestCA(t)
	pres := &recordingPresence{}
	srv := &Server{Presence: pres}
	addr, stop := startMTLSRunnable(t, srv, mtlsServerConfig(ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false), ca.pool))
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // before stop: the server drains open streams on shutdown

	cert := ca.leafWithoutEKU(t, []string{"swarmada-controlstream.system.svc", serverServiceSAN})
	stream, err := tlsClient(t, addr, clientConfig(ca.pool, &cert)).ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	h := hello()
	h.GetHello().Namespace = "system"
	if err := stream.Send(h); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("a certificate with no extended key usage was refused as a client: %v", err)
	}
	if !reply.GetHelloAck().GetAccepted() {
		t.Fatalf("hello refused: %s", reply.GetHelloAck().GetMessage())
	}
	eventually(t, func() bool { return len(pres.snapshot()) == 1 })
	got := pres.snapshot()[0]
	want := TLSIdentity{AdapterName: "swarmada-controlstream", Namespace: "system", Verified: true}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

// With server auth set, the same key is refused at the handshake when presented as a client.
func TestServerAuthCertificateIsRefusedAsAClient(t *testing.T) {
	ca := newTestCA(t)
	addr, stop := startMTLSRunnable(t, &Server{}, mtlsServerConfig(ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false), ca.pool))
	defer stop()
	cert := ca.issue(t, []string{"swarmada-controlstream.system.svc", serverServiceSAN}, time.Now().Add(time.Hour), false)
	assertHandshakeRejected(t, tlsClient(t, addr, clientConfig(ca.pool, &cert)))
}

type certManifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		IsCA      bool     `json:"isCA"`
		Usages    []string `json:"usages"`
		DNSNames  []string `json:"dnsNames"`
		IssuerRef struct {
			Name string `json:"name"`
		} `json:"issuerRef"`
	} `json:"spec"`
}

// Every Certificate in config/ issued by the ControlStream CA names exactly one of server
// auth and client auth, and the ControlStream server certificate names server auth.
func TestControlStreamCACertificatesDeclareTheirUsage(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	var checked, servers int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, doc := range strings.Split(string(raw), "\n---") {
			var c certManifest
			if yaml.Unmarshal([]byte(doc), &c) != nil || c.Kind != "Certificate" ||
				c.Spec.IsCA || c.Spec.IssuerRef.Name != "controlstream-ca-issuer" {
				continue
			}
			checked++
			server := slices.Contains(c.Spec.Usages, "server auth")
			client := slices.Contains(c.Spec.Usages, "client auth")
			if server == client {
				t.Errorf("%s: Certificate %s has usages %v; want exactly one of server auth and client auth",
					path, c.Metadata.Name, c.Spec.Usages)
			}
			if slices.Contains(c.Spec.DNSNames, serverServiceSAN) {
				servers++
				if !server || client {
					t.Errorf("%s: ControlStream server Certificate %s has usages %v; want server auth and not client auth",
						path, c.Metadata.Name, c.Spec.Usages)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if checked == 0 || servers == 0 {
		t.Fatalf("found %d Certificates from the ControlStream CA and %d server certificates under %s; the check read nothing",
			checked, servers, root)
	}
	t.Logf("checked %d Certificates from the ControlStream CA, %d of them the server certificate", checked, servers)
}

// The Helm chart issues its own ControlStream CA, server certificate and adapter client
// certificates, so the same rule is checked on what `helm template` renders: a server
// certificate that verified as a client would be adapter verify-rel-swarmada-controlstream
// in namespace verify-ns. helm is required; a skip here would pass without reading the chart.
func TestChartControlStreamCACertificatesDeclareTheirUsage(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is not on PATH; this test renders deploy/swarmada and cannot run without it (install helm)")
	}
	const (
		issuer    = "verify-rel-swarmada-controlstream-ca-issuer"
		serverSAN = "verify-rel-swarmada-controlstream.verify-ns.svc.cluster.local"
	)
	cmd := exec.Command("helm", "template", "verify-rel", filepath.Join("..", "..", "deploy", "swarmada"),
		"--namespace", "verify-ns",
		"--set", "controlStream.tls.certManager.adapterClients[0].name=sim-fleet-adapter",
		"--set", "controlStream.tls.certManager.adapterClients[0].namespace=warehouse-a",
		"--set", "controlStream.tls.certManager.adapterClients[1].name=acme-adapter",
		"--set", "controlStream.tls.certManager.adapterClients[1].namespace=warehouse-b")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var checked, servers, clients int
	for _, doc := range strings.Split(stdout.String(), "\n---") {
		var c certManifest
		if yaml.Unmarshal([]byte(doc), &c) != nil || c.Kind != "Certificate" ||
			c.Spec.IsCA || c.Spec.IssuerRef.Name != issuer {
			continue
		}
		checked++
		server := slices.Contains(c.Spec.Usages, "server auth")
		client := slices.Contains(c.Spec.Usages, "client auth")
		if server == client {
			t.Errorf("chart Certificate %s has usages %v; want exactly one of server auth and client auth",
				c.Metadata.Name, c.Spec.Usages)
		}
		if slices.Contains(c.Spec.DNSNames, serverSAN) {
			servers++
			if !server || client {
				t.Errorf("chart ControlStream server Certificate %s has usages %v; want server auth and not client auth",
					c.Metadata.Name, c.Spec.Usages)
			}
		} else if client {
			clients++
		}
	}
	if servers != 1 || clients != 2 {
		t.Fatalf("rendered chart: %d Certificates from %s, %d server (want 1), %d client (want 2); the check did not read what it should",
			checked, issuer, servers, clients)
	}
	t.Logf("checked %d chart Certificates from the ControlStream CA: 1 server, %d client", checked, clients)
}
