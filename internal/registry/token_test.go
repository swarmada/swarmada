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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bearer-token caching and the OAuth2 refresh grant.
//
// The cache is the first mutable state on Client, and Client is shared by the FirmwareRollout
// fetcher and the ModelPolicy poller. So the tests here are as much about what must NOT be reused
// (another credential's token, another scope's token, an expired token) as about reuse itself.

// authMock serves a token endpoint plus a scoped API, counting exchanges and refreshes.
type authMock struct {
	mu        sync.Mutex
	exchanges int
	refreshes int
	expiresIn int    // 0 → omit expires_in
	refreshTk string // "" → issue no refresh token
	srv       *httptest.Server
}

func newAuthMock(t *testing.T, expiresIn int, refreshTk string) *authMock {
	t.Helper()
	m := &authMock{expiresIn: expiresIn, refreshTk: refreshTk}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				m.refreshes++
			}
		} else {
			m.exchanges++
		}
		n := m.exchanges + m.refreshes
		m.mu.Unlock()

		body := map[string]any{"token": fmt.Sprintf("tok-%d", n)}
		if m.expiresIn > 0 {
			body["expires_in"] = m.expiresIn
		}
		if m.refreshTk != "" {
			body["refresh_token"] = m.refreshTk
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	// Any API path: 401 with a challenge unless a bearer is presented.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			scope := "repository:" + strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/tags/list"), "/v2/") + ":pull"
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="reg",scope="%s"`, m.srv.URL, scope))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"1.0.0"}})
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *authMock) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exchanges, m.refreshes
}

func (m *authMock) client(now func() time.Time) (string, *Client) {
	host := strings.TrimPrefix(m.srv.URL, "http://")
	return host, &Client{HTTP: m.srv.Client(), Scheme: "http", Now: now}
}

// A token is reused within its scope: two requests, one exchange.
func TestTokenCache_ReusedWithinScope(t *testing.T) {
	m := newAuthMock(t, 300, "")
	host, c := m.client(nil)

	for i := 0; i < 3; i++ {
		if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
			t.Fatalf("ListTags %d: %v", i, err)
		}
	}
	if ex, _ := m.counts(); ex != 1 {
		t.Errorf("token exchanges = %d for 3 requests in one scope, want 1", ex)
	}
}

// A DIFFERENT scope must not reuse the token — a scoped token is refused elsewhere, so reusing it
// would produce intermittent 401s that look like flaky auth.
func TestTokenCache_NotReusedAcrossScopes(t *testing.T) {
	m := newAuthMock(t, 300, "")
	host, c := m.client(nil)

	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.ListTags(context.Background(), host, "models/other", nil); err != nil {
		t.Fatalf("second: %v", err)
	}
	if ex, _ := m.counts(); ex != 2 {
		t.Errorf("token exchanges = %d across two scopes, want 2", ex)
	}
}

// THE PRIVILEGE-LEAK CASE. A token bought with one credential must never be served to a caller
// presenting a different one — otherwise a caller who could not obtain a token rides someone
// else's. Client is shared between the rollout fetcher and the policy poller, so this is real.
func TestTokenCache_NotReusedAcrossCredentials(t *testing.T) {
	m := newAuthMock(t, 300, "")
	host, c := m.client(nil)

	if _, err := c.ListTags(context.Background(), host, "models/item",
		&Credential{Username: "alice", Password: "secret"}); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := c.ListTags(context.Background(), host, "models/item",
		&Credential{Username: "mallory", Password: "guess"}); err != nil {
		t.Fatalf("mallory: %v", err)
	}
	if ex, _ := m.counts(); ex != 2 {
		t.Errorf("token exchanges = %d for two different credentials, want 2 — a cached token "+
			"must not cross credentials", ex)
	}
}

// An expired token is not presented: the margin forces a refresh before it lapses.
func TestTokenCache_ExpiredTokenIsNotReused(t *testing.T) {
	m := newAuthMock(t, 30, "") // 30s TTL, 10s margin
	clock := time.Unix(1000, 0)
	host, c := m.client(func() time.Time { return clock })

	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(25 * time.Second) // inside the 10s margin of a 30s token
	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("second: %v", err)
	}
	if ex, _ := m.counts(); ex != 2 {
		t.Errorf("token exchanges = %d, want 2 (the near-expired token must not be reused)", ex)
	}
}

// With a refresh token, an expiry uses the OAuth2 refresh grant rather than re-sending credentials.
func TestTokenCache_ExpiryUsesRefreshGrant(t *testing.T) {
	m := newAuthMock(t, 30, "refresh-abc")
	clock := time.Unix(1000, 0)
	host, c := m.client(func() time.Time { return clock })

	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(25 * time.Second)
	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("second: %v", err)
	}
	ex, rf := m.counts()
	if rf != 1 {
		t.Errorf("refresh grants = %d, want 1 on expiry when a refresh token was issued", rf)
	}
	if ex != 1 {
		t.Errorf("token exchanges = %d, want 1 — the refresh must replace the credential exchange", ex)
	}
}

// A token response with no expires_in still caches, using the conservative default.
func TestTokenCache_NoExpiresInUsesDefault(t *testing.T) {
	m := newAuthMock(t, 0, "")
	clock := time.Unix(1000, 0)
	host, c := m.client(func() time.Time { return clock })

	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(5 * time.Second) // well inside the 60s default
	if _, err := c.ListTags(context.Background(), host, "models/item", nil); err != nil {
		t.Fatalf("second: %v", err)
	}
	if ex, _ := m.counts(); ex != 1 {
		t.Errorf("token exchanges = %d, want 1 (default TTL should still cache)", ex)
	}
}

// The cache survives concurrent use — Client is shared across controllers.
func TestTokenCache_ConcurrentUse(t *testing.T) {
	m := newAuthMock(t, 300, "")
	host, c := m.client(nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.ListTags(context.Background(), host, "models/item", nil)
		}()
	}
	wg.Wait()
	if ex, _ := m.counts(); ex == 0 {
		t.Error("no token was ever exchanged")
	}
}
