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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Registry bearer-token caching and OAuth2 refresh (Docker Registry v2 token auth).
//
// Without a cache every request re-runs the full 401 → auth-server → retry handshake, which for a
// poller on a short interval is a token exchange per poll against a third-party auth server.
//
// The cache key is realm+service+SCOPE, not the registry. A token is issued FOR a scope
// ("repository:acme/fw:pull"); presenting it for a different repository is refused, so caching by
// registry alone would produce intermittent 401s that look like flaky auth rather than a bug here.

// tokenRefreshMargin is how long before expiry a cached token stops being reused. A token handed
// over in the second it lapses fails at the registry, so the margin buys the round trip.
const tokenRefreshMargin = 10 * time.Second

// defaultTokenTTL applies when the auth server omits expires_in. Deliberately short: guessing LONG
// on an unknown lifetime means presenting dead tokens, and the only cost of guessing short is an
// extra exchange.
const defaultTokenTTL = 60 * time.Second

type cachedToken struct {
	token   string
	refresh string
	expires time.Time
}

// tokenCache is safe for concurrent use: one Client is shared by the FirmwareRollout fetcher and
// the ModelPolicy poller.
type tokenCache struct {
	mu   sync.Mutex
	byID map[string]cachedToken
}

func (t *tokenCache) get(key string, now time.Time) (cachedToken, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.byID[key]
	if !ok {
		return cachedToken{}, false
	}
	return e, true
}

func (t *tokenCache) put(key string, e cachedToken) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID == nil {
		t.byID = map[string]cachedToken{}
	}
	t.byID[key] = e
}

// usable reports whether the cached token can still be presented.
func (e cachedToken) usable(now time.Time) bool {
	return e.token != "" && now.Add(tokenRefreshMargin).Before(e.expires)
}

// cacheKey identifies a cached token by BOTH the challenge and the credential it was bought with.
//
// Keying on the challenge alone is a privilege leak: a caller presenting wrong (or no) credentials
// would hit the entry another caller populated with valid ones and be served a token it could never
// have obtained itself. One Client instance is shared by the FirmwareRollout fetcher and the
// ModelPolicy poller, so that is not hypothetical.
//
// The credential is hashed rather than embedded, so a password never sits in a map key that could
// reach a log or a dump.
func cacheKey(ch bearerChallenge, cred *Credential) string {
	sum := sha256.Sum256([]byte(credentialIdentity(cred)))
	return ch.realm + "|" + ch.service + "|" + ch.scope + "|" + hex.EncodeToString(sum[:8])
}

func credentialIdentity(cred *Credential) string {
	if cred == nil {
		return "anonymous"
	}
	// BearerToken callers never reach the exchange (do() skips it), so only the basic pair matters.
	return "basic\x00" + cred.Username + "\x00" + cred.Password
}

// token returns a bearer for the challenge, reusing a cached one when it is still valid.
//
// Order is deliberate: cache hit → refresh grant → full exchange. The refresh grant exists so a
// long-running poller against a registry issuing short-lived tokens does not fall back to sending
// its basic credentials on every expiry.
func (c *Client) token(ctx context.Context, ch bearerChallenge, cred *Credential) (string, error) {
	key := cacheKey(ch, cred)
	now := c.now()

	if e, ok := c.tokens().get(key, now); ok {
		if e.usable(now) {
			return e.token, nil
		}
		if e.refresh != "" {
			if fresh, err := c.refreshToken(ctx, ch, e.refresh); err == nil {
				c.tokens().put(key, fresh)
				return fresh.token, nil
			}
			// A rejected refresh is not fatal — the credentials may still buy a new token, and
			// falling through is what keeps a rotated refresh token from wedging the client.
		}
	}

	fresh, err := c.exchangeToken(ctx, ch, cred)
	if err != nil {
		return "", err
	}
	c.tokens().put(key, fresh)
	return fresh.token, nil
}

// exchangeToken runs the GET flow: basic (or anonymous) credentials for a scoped token.
func (c *Client) exchangeToken(ctx context.Context, ch bearerChallenge, cred *Credential) (cachedToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ch.realm, nil)
	if err != nil {
		return cachedToken{}, err
	}
	q := req.URL.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	if ch.scope != "" {
		q.Set("scope", ch.scope)
	}
	// Ask for a refresh token so later expiries can use the refresh grant instead of re-sending
	// credentials. Registries that do not support it simply omit one.
	q.Set("offline_token", "true")
	req.URL.RawQuery = q.Encode()
	if cred != nil && (cred.Username != "" || cred.Password != "") {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	return c.readTokenResponse(req, ch)
}

// refreshToken runs the OAuth2 refresh grant (POST, form-encoded) defined by the distribution spec.
func (c *Client) refreshToken(ctx context.Context, ch bearerChallenge, refresh string) (cachedToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	if ch.service != "" {
		form.Set("service", ch.service)
	}
	if ch.scope != "" {
		form.Set("scope", ch.scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.realm, strings.NewReader(form.Encode()))
	if err != nil {
		return cachedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.readTokenResponse(req, ch)
}

func (c *Client) readTokenResponse(req *http.Request, ch bearerChallenge) (cachedToken, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return cachedToken{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cachedToken{}, fmt.Errorf("auth server %s: status %d", ch.realm, resp.StatusCode)
	}
	body, err := readCapped(resp.Body)
	if err != nil {
		return cachedToken{}, err
	}
	var tok struct {
		Token        string `json:"token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return cachedToken{}, fmt.Errorf("parsing token response: %w", err)
	}
	value := tok.Token
	if value == "" {
		value = tok.AccessToken
	}
	if value == "" {
		return cachedToken{}, fmt.Errorf("auth server %s returned no token", ch.realm)
	}
	ttl := defaultTokenTTL
	if tok.ExpiresIn > 0 {
		ttl = time.Duration(tok.ExpiresIn) * time.Second
	}
	return cachedToken{
		token:   value,
		refresh: tok.RefreshToken,
		expires: c.now().Add(ttl),
	}, nil
}
