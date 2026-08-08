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

// Package registry is a minimal read-only OCI Distribution API client (the
// registry REST API, plain HTTPS+JSON — no external OCI dependency). It backs the
// ModelPolicy RegistryWatch trigger (RFC-0001 §9.1.9): list a repository's tags,
// resolve a tag's manifest digest (the artifact checksum), and read the image
// config labels that carry model quality metrics.
//
// Scope (honest): anonymous, HTTP basic, or a pre-supplied bearer token. The
// registry token-auth handshake (401 → auth server → bearer) and multi-arch index
// manifests are not implemented — enough for a localhost/dev or basic-auth
// registry, which is the RegistryWatchConfig example.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout  = 30 * time.Second
	defaultMaxBytes = 4 << 20 // 4 MiB cap on a manifest/config body
	// digestHeader carries the canonical content digest of a manifest.
	digestHeader = "Docker-Content-Digest"
	// manifestAccept advertises the manifest media types this client understands.
	manifestAccept = "application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.docker.distribution.manifest.v2+json," +
		"application/vnd.oci.image.index.v1+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json"
)

// Credential authenticates a registry request. A BearerToken takes precedence over
// Username/Password. The zero value is anonymous.
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

// Client talks the OCI Distribution API. Its zero value is usable.
type Client struct {
	// HTTP overrides the client (tests inject an httptest-backed one).
	HTTP *http.Client
	// Scheme overrides the URL scheme. Empty auto-selects: http for a localhost
	// registry, https otherwise.
	Scheme string
	// Platform selects the image from a multi-arch index manifest, as "os/arch".
	// Empty means defaultPlatform. A robot's firmware is arch-specific, so this is
	// never guessed from an index's first entry.
	Platform string
	// Now overrides the clock for token-expiry tests.
	Now func() time.Time

	cache tokenCache
}

// defaultPlatform is the image selected from a multi-arch index when none is configured. Chosen
// explicitly rather than from runtime.GOARCH: the control plane's own architecture says nothing
// about the robots it dispatches to, and inheriting it would make a rollout's contents depend on
// where the manager happens to run.
const defaultPlatform = "linux/amd64"

func (c *Client) tokens() *tokenCache { return &c.cache }

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) platform() string {
	if c.Platform != "" {
		return c.Platform
	}
	return defaultPlatform
}

// New returns a production registry client.
func New() *Client { return &Client{} }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// scheme picks the URL scheme for a registry host: an explicit override wins,
// else http for a loopback dev registry (the localhost:5000 example) and https
// for everything else.
func (c *Client) scheme(registry string) string {
	if c.Scheme != "" {
		return c.Scheme
	}
	host := registry
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "http"
	}
	return "https"
}

func (c *Client) do(ctx context.Context, registry, path, accept string, cred *Credential) (*http.Response, error) {
	url := c.scheme(registry) + "://" + registry + path

	resp, err := c.get(ctx, url, accept, "", cred)
	if err != nil {
		return nil, fmt.Errorf("registry GET %s: %w", path, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	// Docker Registry v2 token auth (§ distribution-spec): a 401 with a Bearer
	// challenge means we must exchange (basic/anonymous) credentials for a scoped
	// token at the challenge's realm, then retry. Only attempt when we did not
	// already present a bearer, so a bad/expired token can't loop.
	if resp.StatusCode == http.StatusUnauthorized && (cred == nil || cred.BearerToken == "") {
		challenge, ok := parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
		_ = resp.Body.Close()
		if ok {
			token, terr := c.token(ctx, challenge, cred)
			if terr != nil {
				return nil, fmt.Errorf("registry GET %s: token auth: %w", path, terr)
			}
			retry, rerr := c.get(ctx, url, accept, token, nil)
			if rerr != nil {
				return nil, fmt.Errorf("registry GET %s (authed): %w", path, rerr)
			}
			if retry.StatusCode >= 200 && retry.StatusCode < 300 {
				return retry, nil
			}
			_ = retry.Body.Close()
			return nil, fmt.Errorf("registry GET %s: unexpected status %d after token auth", path, retry.StatusCode)
		}
	}
	_ = resp.Body.Close()
	return nil, fmt.Errorf("registry GET %s: unexpected status %d", path, resp.StatusCode)
}

// get issues one GET. A non-empty bearer sets the Authorization header directly;
// otherwise cred is applied (basic/bearer/anonymous).
func (c *Client) get(ctx context.Context, url, accept, bearer string, cred *Credential) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		cred.apply(req)
	}
	return c.httpClient().Do(req)
}

// bearerChallenge is a parsed WWW-Authenticate: Bearer challenge.
type bearerChallenge struct {
	realm   string
	service string
	scope   string
}

// parseBearerChallenge parses a `Bearer realm="...",service="...",scope="..."`
// header. It returns ok=false for any non-Bearer scheme or a missing realm.
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return bearerChallenge{}, false
	}
	var ch bearerChallenge
	for _, part := range splitChallengeParams(strings.TrimPrefix(header, "Bearer ")) {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(key) {
		case "realm":
			ch.realm = val
		case "service":
			ch.service = val
		case "scope":
			ch.scope = val
		}
	}
	if ch.realm == "" {
		return bearerChallenge{}, false
	}
	return ch, true
}

// splitChallengeParams splits on commas that are not inside a quoted value.
func splitChallengeParams(s string) []string {
	var parts []string
	var buf strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			buf.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

// fetchToken exchanges credentials for a scoped bearer token at the challenge
// realm (GET realm?service=&scope=). Basic credentials, when present, authenticate
// to the AUTH SERVER (not the registry API); a public repo yields an anonymous
// token. It reads `token` or `access_token` from the JSON response.
func readCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, defaultMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > defaultMaxBytes {
		return nil, fmt.Errorf("registry response exceeds the %d-byte cap", defaultMaxBytes)
	}
	return body, nil
}

// ListTags returns the tags of a repository (§ tags/list).
func (c *Client) ListTags(ctx context.Context, registry, repo string, cred *Credential) ([]string, error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/tags/list", "", cred)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parsing tags list: %w", err)
	}
	return out.Tags, nil
}

// Descriptor resolves a tag (or digest) to its manifest digest (the artifact
// checksum, from the Docker-Content-Digest header) and the config-blob digest.
func (c *Client) Descriptor(ctx context.Context, registry, repo, ref string, cred *Credential) (manifestDigest, configDigest string, err error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/manifests/"+ref, manifestAccept, cred)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return "", "", err
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", "", fmt.Errorf("parsing manifest: %w", err)
	}
	return resp.Header.Get(digestHeader), m.Config.Digest, nil
}

// Layer is one entry of an image/artifact manifest's layers[].
type Layer struct {
	Digest    string
	MediaType string
	// Annotations carries the layer's OCI annotations. Required for cosign artifacts, which do NOT
	// put the signature in the blob: the blob is a simplesigning payload and the signature itself
	// lives in the `dev.cosignproject.cosign/signature` annotation.
	Annotations map[string]string
}

// Layers resolves a manifest (by tag or digest) to its layer descriptors. Used to
// find a detached signature stored as a layer of an OCI artifact.
func (c *Client) Layers(ctx context.Context, registry, repo, ref string, cred *Credential) ([]Layer, error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/manifests/"+ref, manifestAccept, cred)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Layers    []struct {
			Digest      string            `json:"digest"`
			MediaType   string            `json:"mediaType"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
		// Manifests is the multi-arch INDEX form: no layers of its own, just per-platform
		// pointers. Present on an OCI image index / Docker manifest list.
		Manifests []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Platform  struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest layers: %w", err)
	}

	// Multi-arch index: resolve to the platform's image manifest and return ITS layers.
	//
	// Selecting deliberately, never "the first entry": a robot's firmware is architecture-specific,
	// so silently picking the wrong image would hand a device bytes it cannot run — and it would
	// pass every digest and signature check on the way, because those bytes are genuinely signed.
	// No match is an ERROR for the same reason.
	if len(m.Manifests) > 0 && len(m.Layers) == 0 {
		want := c.platform()
		var target string
		for _, entry := range m.Manifests {
			if entry.Platform.OS+"/"+entry.Platform.Architecture == want {
				target = entry.Digest
				break
			}
		}
		if target == "" {
			available := make([]string, 0, len(m.Manifests))
			for _, entry := range m.Manifests {
				available = append(available, entry.Platform.OS+"/"+entry.Platform.Architecture)
			}
			return nil, fmt.Errorf("index manifest %s/%s:%s has no %s image (available: %s)",
				registry, repo, ref, want, strings.Join(available, ", "))
		}
		if target == ref {
			return nil, fmt.Errorf("index manifest %s/%s:%s points at itself", registry, repo, ref)
		}
		return c.Layers(ctx, registry, repo, target, cred)
	}
	out := make([]Layer, 0, len(m.Layers))
	for _, l := range m.Layers {
		out = append(out, Layer{Digest: l.Digest, MediaType: l.MediaType, Annotations: l.Annotations})
	}
	return out, nil
}

// Blob fetches a content-addressed blob by digest (§ blobs/<digest>) and returns
// its raw bytes. Used to retrieve a detached artifact signature stored as an OCI
// blob. The caller is responsible for verifying the bytes (digest + signature) —
// this is untrusted transport.
func (c *Client) Blob(ctx context.Context, registry, repo, digest string, cred *Credential) ([]byte, error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/blobs/"+digest, "", cred)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readCapped(resp.Body)
}

// ConfigLabels fetches an image config blob and returns its config.Labels map
// (where the RegistryWatch metrics label lives).
func (c *Client) ConfigLabels(ctx context.Context, registry, repo, configDigest string, cred *Credential) (map[string]string, error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/blobs/"+configDigest, "", cred)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parsing image config: %w", err)
	}
	return cfg.Config.Labels, nil
}

// ConfigCreated returns the image config's `created` timestamp — the BUILD time.
//
// Used only as a tie-break when version ordering cannot separate two tags (see HighestByTime); it
// is not push time and must not be treated as one. A zero time means "no usable signal": the field
// was absent, unparseable, or pinned to the epoch by a reproducible build.
func (c *Client) ConfigCreated(ctx context.Context, registry, repo, configDigest string, cred *Credential) (time.Time, error) {
	resp, err := c.do(ctx, registry, "/v2/"+repo+"/blobs/"+configDigest, "", cred)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readCapped(resp.Body)
	if err != nil {
		return time.Time{}, err
	}
	var cfg struct {
		Created string `json:"created"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return time.Time{}, fmt.Errorf("parsing image config: %w", err)
	}
	if cfg.Created == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, cfg.Created)
	if err != nil {
		return time.Time{}, nil // unparseable is "no signal", not an error worth failing a poll for
	}
	if at.Unix() <= 0 {
		return time.Time{}, nil // epoch-pinned reproducible build: carries no ordering information
	}
	return at, nil
}
