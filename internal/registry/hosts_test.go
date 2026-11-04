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

package registry

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Where a registry credential may go: the registry hosts it lists, a token server it lists,
// over https or on loopback, and nowhere a redirect points that it does not list.

// authRecorder is a server that records the Authorization header of every request and
// answers a token request with a token.
type authRecorder struct {
	mu   sync.Mutex
	auth []string
	srv  *httptest.Server
}

func newAuthRecorder(t *testing.T, tlsServer bool) *authRecorder {
	t.Helper()
	a := &authRecorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		a.mu.Unlock()
		_, _ = w.Write([]byte(`{"token":"granted","tags":[]}`))
	})
	if tlsServer {
		a.srv = httptest.NewTLSServer(h)
	} else {
		a.srv = httptest.NewServer(h)
	}
	t.Cleanup(a.srv.Close)
	return a
}

func (a *authRecorder) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auth...)
}

func (a *authRecorder) hostPort() string {
	return strings.TrimPrefix(strings.TrimPrefix(a.srv.URL, "https://"), "http://")
}

// challengingRegistry is a plain-http loopback registry that challenges to realm until a
// bearer is presented.
func challengingRegistry(t *testing.T, realm string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="reg",scope="repository:r:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tags":["1.0.0"]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// routedClient dials named hosts to local servers (name:port -> address) and every other
// address as given. Verification is off: the local certificate names none of the hosts.
func routedClient(routes map[string]string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if to, ok := routes[addr]; ok {
				addr = to
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client for local servers
	}}
}

// A token server reached over plain http (not loopback) receives no credentials.
func TestTokenAuth_PlainHTTPRealmReceivesNoCredentials(t *testing.T) {
	realm := newAuthRecorder(t, false)
	reg := challengingRegistry(t, "http://auth.example/token")
	host := strings.TrimPrefix(reg.URL, "http://")
	c := &Client{HTTP: routedClient(map[string]string{"auth.example:80": realm.hostPort()}), Scheme: "http"}

	_, err := c.ListTags(context.Background(), host, "r",
		&Credential{Username: "u", Password: "p", Hosts: []string{host, "auth.example"}})
	if err == nil {
		t.Error("a token exchange over plain http to a non-loopback realm succeeded")
	}
	for _, got := range realm.received() {
		if got != "" {
			t.Fatalf("plain-http token server received Authorization %q", got)
		}
	}
}

// A token server on a host the credential does not list gets an anonymous exchange.
func TestTokenAuth_RealmOnUnlistedHostIsExchangedAnonymously(t *testing.T) {
	realm := newAuthRecorder(t, true)
	reg := challengingRegistry(t, "https://auth.example/token")
	host := strings.TrimPrefix(reg.URL, "http://")
	c := &Client{HTTP: routedClient(map[string]string{"auth.example:443": realm.hostPort()}), Scheme: "http"}

	if _, err := c.ListTags(context.Background(), host, "r",
		&Credential{Username: "u", Password: "p", Hosts: []string{host}}); err != nil {
		t.Fatalf("anonymous exchange at an unlisted realm: %v", err)
	}
	got := realm.received()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("unlisted token server received Authorization %q, want one anonymous request", got)
	}
}

// A token server the credential lists receives the credentials over https.
func TestTokenAuth_ListedHTTPSRealmReceivesCredentials(t *testing.T) {
	realm := newAuthRecorder(t, true)
	reg := challengingRegistry(t, "https://auth.example/token")
	host := strings.TrimPrefix(reg.URL, "http://")
	c := &Client{HTTP: routedClient(map[string]string{"auth.example:443": realm.hostPort()}), Scheme: "http"}

	if _, err := c.ListTags(context.Background(), host, "r",
		&Credential{Username: "u", Password: "p", Hosts: []string{host, "auth.example"}}); err != nil {
		t.Fatal(err)
	}
	if got := realm.received(); len(got) != 1 || !strings.HasPrefix(got[0], "Basic ") {
		t.Errorf("listed token server received %q, want the basic credentials", got)
	}
}

// The loopback development registry may use a plain-http token server.
func TestTokenAuth_LoopbackHTTPRealmIsAllowed(t *testing.T) {
	realm := newAuthRecorder(t, false)
	reg := challengingRegistry(t, realm.srv.URL+"/token")
	host := strings.TrimPrefix(reg.URL, "http://")
	c := &Client{HTTP: reg.Client(), Scheme: "http"}

	if _, err := c.ListTags(context.Background(), host, "r",
		&Credential{Username: "u", Password: "p", Hosts: []string{host, realm.hostPort()}}); err != nil {
		t.Fatalf("loopback http realm: %v", err)
	}
	if got := realm.received(); len(got) != 1 || !strings.HasPrefix(got[0], "Basic ") {
		t.Errorf("loopback token server received %q, want the basic credentials", got)
	}
}

// A registry the credential does not list is read anonymously.
func TestClient_CredentialOnlyToListedRegistry(t *testing.T) {
	reg := newAuthRecorder(t, false)
	c := &Client{HTTP: reg.srv.Client(), Scheme: "http"}

	_, _ = c.ListTags(context.Background(), reg.hostPort(), "r",
		&Credential{BearerToken: "tok", Hosts: []string{"other.example"}})
	_, _ = c.ListTags(context.Background(), reg.hostPort(), "r", &Credential{BearerToken: "tok"})
	for _, got := range reg.received() {
		if got != "" {
			t.Fatalf("unlisted registry received Authorization %q", got)
		}
	}
}

// A redirect from the registry to another host is followed without the credential.
func TestClient_RedirectToUnlistedHostDropsCredential(t *testing.T) {
	storage := newAuthRecorder(t, false)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.srv.URL+"/blob", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(reg.Close)
	host := strings.TrimPrefix(reg.URL, "http://")
	c := &Client{HTTP: reg.Client(), Scheme: "http"}

	if _, err := c.Blob(context.Background(), host, "r", "sha256:abc",
		&Credential{BearerToken: "tok", Hosts: []string{host}}); err != nil {
		t.Fatalf("redirected blob: %v", err)
	}
	got := storage.received()
	if len(got) != 1 || got[0] != "" {
		t.Errorf("redirect target received Authorization %q, want one anonymous request", got)
	}
}
