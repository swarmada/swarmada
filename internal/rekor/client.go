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

// Package rekor is a minimal client for the sigstore Rekor transparency log
// (RFC-0001 §9.1.12): it checks whether an artifact hash has a log entry, the
// first-level transparency requirement gating a signed-artifact dispatch.
//
// Scope (honest): a presence check via the search-index API. Full cryptographic
// verification of the entry — inclusion proof and signed-entry-timestamp (SET) —
// is a follow-on; this proves the artifact hash is indexed, not that Rekor's log
// is consistent. The trust-root signature check remains the primary gate.
package rekor

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout  = 30 * time.Second
	defaultMaxBytes = 1 << 20 // 1 MiB cap on the UUID list
)

// Checker reports whether a hash has a Rekor transparency-log entry.
//
// VerifyEntry is the gate callers should use: HasEntry proves only that the endpoint indexed the
// hash, which a hostile server satisfies trivially. VerifyEntry additionally proves the entry is in
// the log and was signed by it, when a log key is pinned.
type Checker interface {
	HasEntry(ctx context.Context, rekorURL, hash string) (bool, error)
	VerifyEntry(ctx context.Context, rekorURL, hash string, logKey crypto.PublicKey) (string, error)
}

// Client queries a Rekor server over https.
type Client struct {
	// HTTP overrides the client (tests inject an httptest-backed one).
	HTTP *http.Client
}

// New returns a production Rekor client.
func New() *Client { return &Client{} }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// entryUUIDs returns the UUIDs the search index holds for a hash. Shared with VerifyEntry
// (verify.go) so the two paths cannot diverge on URL handling, the https requirement, or the
// response-size cap.
func (c *Client) entryUUIDs(ctx context.Context, rekorURL, hash string) ([]string, error) {
	base, err := url.Parse(strings.TrimRight(rekorURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing rekorUrl %q: %w", rekorURL, err)
	}
	if base.Scheme != "https" {
		return nil, fmt.Errorf("rekorUrl %q must be https", rekorURL)
	}
	body, err := json.Marshal(map[string]string{"hash": hash})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base.String()+"/api/v1/index/retrieve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying rekor %q: %w", rekorURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rekor %q: unexpected status %d", rekorURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > defaultMaxBytes {
		return nil, fmt.Errorf("rekor %q: response exceeds the %d-byte cap", rekorURL, defaultMaxBytes)
	}
	var uuids []string
	if err := json.Unmarshal(raw, &uuids); err != nil {
		return nil, fmt.Errorf("parsing rekor index response: %w", err)
	}
	return uuids, nil
}

// HasEntry reports whether the Rekor log at rekorURL has at least one entry
// indexed by hash (accepted as `sha256:<hex>` or bare hex). It POSTs to the
// search-index API and treats a non-empty UUID array as "present". A non-https
// rekorURL, a transport error, or a non-2xx response is an error (fail closed at
// the caller).
func (c *Client) HasEntry(ctx context.Context, rekorURL, hash string) (bool, error) {
	base, err := url.Parse(strings.TrimRight(rekorURL, "/"))
	if err != nil {
		return false, fmt.Errorf("parsing rekorUrl %q: %w", rekorURL, err)
	}
	if base.Scheme != "https" {
		return false, fmt.Errorf("rekorUrl %q must be https", rekorURL)
	}

	body, err := json.Marshal(map[string]string{"hash": hash})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/api/v1/index/retrieve", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("querying rekor %q: %w", rekorURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("rekor %q: unexpected status %d", rekorURL, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(raw)) > defaultMaxBytes {
		return false, fmt.Errorf("rekor %q: response exceeds the %d-byte cap", rekorURL, defaultMaxBytes)
	}
	var uuids []string
	if err := json.Unmarshal(raw, &uuids); err != nil {
		return false, fmt.Errorf("parsing rekor index response: %w", err)
	}
	return len(uuids) > 0, nil
}
