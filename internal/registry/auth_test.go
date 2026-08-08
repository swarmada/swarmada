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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tokenAuthMock is a registry that requires a Bearer token: the API 401s with a
// challenge pointing at its own /token endpoint, which issues "sekret".
func tokenAuthMock(t *testing.T, wantAuthHeader string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if wantAuthHeader != "" && r.Header.Get("Authorization") != wantAuthHeader {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("scope") == "" || r.URL.Query().Get("service") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"token":"sekret"}`))
	})
	mux.HandleFunc("/v2/models/item/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sekret" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="reg.example",scope="repository:models/item:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tags":["1.0.0"]}`))
	})
	srv = httptest.NewServer(mux)
	return srv
}

func TestTokenAuth_ChallengeThenRetry(t *testing.T) {
	ts := tokenAuthMock(t, "")
	defer ts.Close()
	host, c := hostOf(ts)

	tags, err := c.ListTags(context.Background(), host, "models/item", nil)
	if err != nil {
		t.Fatalf("ListTags with token auth: %v", err)
	}
	if len(tags) != 1 || tags[0] != "1.0.0" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestTokenAuth_BasicCredentialsForwardedToAuthServer(t *testing.T) {
	// The auth server requires basic creds; the API endpoint never sees them.
	ts := tokenAuthMock(t, "Basic dTpw") // base64("u:p")
	defer ts.Close()
	host, c := hostOf(ts)

	if _, err := c.ListTags(context.Background(), host, "models/item", &Credential{Username: "u", Password: "p"}); err != nil {
		t.Fatalf("token auth with basic creds: %v", err)
	}
	// Wrong creds → the auth server 401s → the whole request fails.
	if _, err := c.ListTags(context.Background(), host, "models/item", &Credential{Username: "u", Password: "wrong"}); err == nil {
		t.Fatal("expected failure when the auth server rejects the basic credentials")
	}
}

func TestTokenAuth_PreSuppliedBearerDoesNotLoop(t *testing.T) {
	// The API always 401s; a caller-supplied (wrong) bearer must NOT trigger the
	// token exchange — it should fail once, not loop.
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://unused/token",service="s"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	host, c := hostOf(ts)

	if _, err := c.ListTags(context.Background(), host, "r", &Credential{BearerToken: "stale"}); err == nil {
		t.Fatal("expected an error for a stale pre-supplied bearer")
	}
	if calls != 1 {
		t.Errorf("made %d API calls, want 1 (no token-exchange loop when a bearer was already sent)", calls)
	}
}

func TestParseBearerChallenge(t *testing.T) {
	ch, ok := parseBearerChallenge(`Bearer realm="https://auth.example/token",service="reg.example",scope="repository:foo/bar:pull"`)
	if !ok || ch.realm != "https://auth.example/token" || ch.service != "reg.example" || ch.scope != "repository:foo/bar:pull" {
		t.Fatalf("challenge = %+v ok=%v", ch, ok)
	}
	// A scope value containing a comma stays intact (split honours quotes).
	ch2, ok := parseBearerChallenge(`Bearer realm="https://a/token",scope="repository:x:pull,push"`)
	if !ok || ch2.scope != "repository:x:pull,push" {
		t.Fatalf("quoted-comma scope = %q ok=%v", ch2.scope, ok)
	}
	// Non-Bearer / missing realm → not ok.
	if _, ok := parseBearerChallenge(`Basic realm="x"`); ok {
		t.Error("Basic challenge should not parse as Bearer")
	}
	if _, ok := parseBearerChallenge(`Bearer service="s"`); ok {
		t.Error("a challenge without realm should not parse")
	}
}

func TestTokenAuth_UsesAccessTokenField(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"viaAccessToken"}`)) // OAuth2-style field
	})
	mux.HandleFunc("/v2/r/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer viaAccessToken" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="s"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tags":[]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	c := &Client{HTTP: srv.Client(), Scheme: "http"}

	if _, err := c.ListTags(context.Background(), host, "r", nil); err != nil {
		t.Fatalf("access_token field not honoured: %v", err)
	}
}
