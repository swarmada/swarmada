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

// Package artifact fetches update-artifact material (detached signatures today)
// referenced by a FirmwareRollout/ModelRollout so the control plane can verify it
// against a trust root before dispatch (RFC-0001 §9.2.8).
//
// The fetcher is untrusted transport: what it returns is trusted ONLY after
// [github.com/swarmada/swarmada/internal/signing].Verify succeeds against a
// configured trust root, so a hostile URL, redirect, or man-in-the-middle cannot
// inject a valid signature. It is https-only (defence against SSRF to plaintext
// internal endpoints and against file:// reads) with a request timeout and a
// response-size cap.
package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrUnsupportedScheme is returned for a reference the HTTP fetcher does not
// handle (notably oci://, which needs a registry client that is not yet wired).
// Callers distinguish it from a transport failure to fail closed with an honest
// "not yet wired" message rather than a spurious network error.
var ErrUnsupportedScheme = errors.New("unsupported artifact reference scheme")

const (
	// defaultTimeout bounds a single artifact fetch.
	defaultTimeout = 30 * time.Second
	// defaultMaxBytes caps a fetched artifact (a detached signature is tiny; this
	// is a DoS backstop against a hostile endpoint streaming forever).
	defaultMaxBytes = 1 << 20 // 1 MiB
)

// Credential authenticates a registry/artifact fetch. A BearerToken takes
// precedence over Username/Password. The zero value is anonymous.
type Credential struct {
	BearerToken string
	Username    string
	Password    string
}

func (c *Credential) apply(req *http.Request) {
	if c == nil {
		return
	}
	switch {
	case c.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	case c.Username != "" || c.Password != "":
		req.SetBasicAuth(c.Username, c.Password)
	}
}

// Fetcher retrieves the bytes an artifact reference points at.
type Fetcher interface {
	// Fetch returns the raw bytes at ref, applying cred (nil = anonymous). It
	// returns ErrUnsupportedScheme (wrapped) for a scheme it cannot handle.
	Fetch(ctx context.Context, ref string, cred *Credential) ([]byte, error)
}

// HTTPFetcher fetches https artifact references. Its zero value is usable
// (default client, timeout, and size cap).
type HTTPFetcher struct {
	// Client overrides the HTTP client (tests inject an httptest-backed one). Nil
	// means a default client with defaultTimeout.
	Client *http.Client
	// MaxBytes overrides the response size cap. Zero means defaultMaxBytes.
	MaxBytes int64
}

// DefaultFetcher returns the production https fetcher.
func DefaultFetcher() *HTTPFetcher { return &HTTPFetcher{} }

func (f *HTTPFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (f *HTTPFetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return defaultMaxBytes
}

// Fetch retrieves ref over https. A non-https scheme (including oci://) returns
// ErrUnsupportedScheme; a non-2xx response or an over-cap body is an error.
func (f *HTTPFetcher) Fetch(ctx context.Context, ref string, cred *Credential) ([]byte, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("parsing artifact ref %q: %w", ref, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q (only https is wired)", ErrUnsupportedScheme, u.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %q: %w", ref, err)
	}
	cred.apply(req)

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %q: %w", ref, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching %q: unexpected status %d", ref, resp.StatusCode)
	}

	// Read one byte past the cap so an exactly-at-cap body still succeeds while an
	// over-cap body is rejected rather than silently truncated.
	limit := f.maxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", ref, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("artifact %q exceeds the %d-byte cap", ref, limit)
	}
	return body, nil
}
