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

package artifact

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// namedHosts is one TLS server answering for every host name, with a client that dials it
// whatever the URL says. It records the Authorization header each host received, so a
// test can name hosts (a.example, sub.a.example, b.example) without DNS.
type namedHosts struct {
	srv    *httptest.Server
	mu     sync.Mutex
	auth   map[string][]string // host -> Authorization values received, in order
	routes map[string]string   // host+path -> redirect target
}

func newNamedHosts(t *testing.T) *namedHosts {
	t.Helper()
	n := &namedHosts{auth: map[string][]string{}, routes: map[string]string{}}
	n.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.auth[r.Host] = append(n.auth[r.Host], r.Header.Get("Authorization"))
		to, redirect := n.routes[r.Host+r.URL.Path]
		n.mu.Unlock()
		if redirect {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("sig"))
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// client dials the one server for every host; the certificate names none of them, so
// verification is off in this test client only.
func (n *namedHosts) client() *http.Client {
	addr := n.srv.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client for a local server
	}}
}

func (n *namedHosts) received(host string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.auth[host]...)
}

func (n *namedHosts) redirect(fromHostPath, to string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[fromHostPath] = to
}

func TestParseRegistries(t *testing.T) {
	got := ParseRegistries(" Reg.Example:5000 ,ghcr.io\nartifacts.example.com\r\n,, ")
	want := []string{"reg.example:5000", "ghcr.io", "artifacts.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseRegistries = %q, want %q", got, want)
	}
	if got := ParseRegistries(""); len(got) != 0 {
		t.Fatalf("empty value lists %q, want nothing", got)
	}
}

func TestCredentialFor_ExactHostOnly(t *testing.T) {
	c := &Credential{BearerToken: "t", Registries: []string{"a.example", "reg.example:5000"}}
	for host, want := range map[string]bool{
		"a.example":        true,
		"A.Example":        true,
		"reg.example:5000": true,
		"sub.a.example":    false,
		"a.example:8443":   false,
		"reg.example":      false,
		"b.example":        false,
		"":                 false,
	} {
		if got := c.For(host) != nil; got != want {
			t.Errorf("For(%q) presents the credential = %v, want %v", host, got, want)
		}
	}
	if (&Credential{BearerToken: "t"}).For("a.example") != nil {
		t.Error("a credential that lists no host was presented")
	}
}

// A credential reaches the hosts it lists and no other.
func TestHTTPFetcher_CredentialGoesOnlyToListedHosts(t *testing.T) {
	n := newNamedHosts(t)
	f := &HTTPFetcher{Client: n.client()}
	cred := &Credential{BearerToken: "tok", Registries: []string{"a.example"}}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig", cred); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), "https://b.example/x.sig", cred); err != nil {
		t.Fatal(err)
	}
	if got := n.received("a.example"); len(got) != 1 || got[0] != "Bearer tok" {
		t.Errorf("listed host received %q, want the credential", got)
	}
	if got := n.received("b.example"); len(got) != 1 || got[0] != "" {
		t.Errorf("unlisted host received %q, want an anonymous request", got)
	}
}

// A credential that lists no host is never sent.
func TestHTTPFetcher_CredentialWithoutRegistriesIsNeverSent(t *testing.T) {
	n := newNamedHosts(t)
	f := &HTTPFetcher{Client: n.client()}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig", &Credential{Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if got := n.received("a.example"); len(got) != 1 || got[0] != "" {
		t.Errorf("host received %q from a credential that lists no host, want nothing", got)
	}
}

// A credentialed fetch redirected to another host follows the redirect without the
// credential: the fetch succeeds, and that host receives one anonymous request.
func TestHTTPFetcher_CredentialNotCarriedAcrossRedirectToAnotherHost(t *testing.T) {
	n := newNamedHosts(t)
	n.redirect("a.example/x.sig", "https://b.example/x.sig")
	f := &HTTPFetcher{Client: n.client()}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig",
		&Credential{BearerToken: "tok", Registries: []string{"a.example"}}); err != nil {
		t.Fatalf("a redirect to an unlisted host must be followed anonymously: %v", err)
	}
	if got := n.received("b.example"); len(got) != 1 || got[0] != "" {
		t.Fatalf("redirect target received %q, want one anonymous request", got)
	}
}

// A subdomain of a listed host is not listed. net/http would carry the header there on its
// own; the fetch follows the redirect and the subdomain receives no credential.
func TestHTTPFetcher_CredentialDroppedOnRedirectToUnlistedSubdomain(t *testing.T) {
	n := newNamedHosts(t)
	n.redirect("a.example/x.sig", "https://sub.a.example/x.sig")
	f := &HTTPFetcher{Client: n.client()}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig",
		&Credential{BearerToken: "tok", Registries: []string{"a.example"}}); err != nil {
		t.Fatalf("a redirect to an unlisted subdomain must be followed anonymously: %v", err)
	}
	if got := n.received("sub.a.example"); len(got) != 1 || got[0] != "" {
		t.Errorf("unlisted subdomain received %q, want one anonymous request", got)
	}
}

// A redirect between two listed hosts is followed with the credential.
func TestHTTPFetcher_CredentialedRedirectBetweenListedHostsIsFollowed(t *testing.T) {
	n := newNamedHosts(t)
	n.redirect("a.example/x.sig", "https://b.example/x.sig")
	f := &HTTPFetcher{Client: n.client()}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig",
		&Credential{BearerToken: "tok", Registries: []string{"a.example", "b.example"}}); err != nil {
		t.Fatalf("redirect between listed hosts: %v", err)
	}
	if got := n.received("b.example"); len(got) != 1 {
		t.Errorf("listed redirect target received %d requests, want 1", len(got))
	}
}

// An anonymous fetch follows redirects as before.
func TestHTTPFetcher_AnonymousRedirectIsFollowed(t *testing.T) {
	n := newNamedHosts(t)
	n.redirect("a.example/x.sig", "https://sub.a.example/x.sig")
	f := &HTTPFetcher{Client: n.client()}

	if _, err := f.Fetch(context.Background(), "https://a.example/x.sig", nil); err != nil {
		t.Fatalf("anonymous redirect: %v", err)
	}
}

// An entry written as a URL is reported, and matches no host.
func TestURLShapedEntries(t *testing.T) {
	hosts := ParseRegistries("https://a.example, b.example:5000,oci://c.example")
	if got, want := URLShapedEntries(hosts), []string{"https://a.example", "oci://c.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("URLShapedEntries = %q, want %q", got, want)
	}
	if HostListed(hosts, "a.example") || HostListed(hosts, "c.example") {
		t.Error("an entry written as a URL matched a host")
	}
	if got := URLShapedEntries(ParseRegistries("a.example,b.example:5000")); len(got) != 0 {
		t.Errorf("a list of hosts reported %q", got)
	}
}
