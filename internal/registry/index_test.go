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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Multi-arch index manifests.
//
// The rule that matters is that selection is DELIBERATE. A robot's firmware is
// architecture-specific, so picking "the first entry" would hand a device bytes it cannot run —
// and those bytes would pass every digest and signature check on the way, because they are
// genuinely signed. No match is therefore an error, never a fallback.

const (
	amdDigest = "sha256:" + "aa11111111111111111111111111111111111111111111111111111111111111"
	armDigest = "sha256:" + "bb22222222222222222222222222222222222222222222222222222222222222"
)

// indexServer serves an index at :latest plus the two per-platform manifests.
func indexServer(t *testing.T, platforms map[string]string) (string, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch ref {
		case "latest":
			entries := make([]map[string]any, 0, len(platforms))
			for plat, digest := range platforms {
				parts := strings.SplitN(plat, "/", 2)
				entries = append(entries, map[string]any{
					"digest":    digest,
					"mediaType": "application/vnd.oci.image.manifest.v1+json",
					"platform":  map[string]string{"os": parts[0], "architecture": parts[1]},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mediaType": "application/vnd.oci.image.index.v1+json",
				"manifests": entries,
			})
		default:
			// A per-platform image manifest, with a layer naming its digest so the test can tell
			// which platform was resolved.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"layers": []map[string]any{
					{"digest": ref, "mediaType": "application/vnd.oci.image.layer.v1.tar"},
				},
			})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &Client{HTTP: srv.Client(), Scheme: "http"}
}

// The configured platform's image is resolved, not the first entry.
func TestIndexManifest_ResolvesConfiguredPlatform(t *testing.T) {
	host, c := indexServer(t, map[string]string{"linux/amd64": amdDigest, "linux/arm64": armDigest})
	c.Platform = "linux/arm64"

	layers, err := c.Layers(context.Background(), host, "acme/fw", "latest", nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 1 || layers[0].Digest != armDigest {
		t.Fatalf("resolved layers = %+v, want the arm64 image %s", layers, armDigest)
	}
}

// The default is linux/amd64 when none is configured.
func TestIndexManifest_DefaultPlatform(t *testing.T) {
	host, c := indexServer(t, map[string]string{"linux/amd64": amdDigest, "linux/arm64": armDigest})

	layers, err := c.Layers(context.Background(), host, "acme/fw", "latest", nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if layers[0].Digest != amdDigest {
		t.Errorf("default resolved %s, want the linux/amd64 image %s", layers[0].Digest, amdDigest)
	}
}

// No matching platform is an ERROR — never a silent fallback to another architecture.
func TestIndexManifest_NoMatchingPlatformErrors(t *testing.T) {
	host, c := indexServer(t, map[string]string{"linux/arm64": armDigest})
	c.Platform = "linux/amd64"

	_, err := c.Layers(context.Background(), host, "acme/fw", "latest", nil)
	if err == nil {
		t.Fatal("an index with no matching platform must error, not fall back to another arch")
	}
	if !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("error = %q, want it to list what IS available", err.Error())
	}
}

// A plain (non-index) manifest still works unchanged.
func TestIndexManifest_PlainManifestUnaffected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"layers": []map[string]any{
				{"digest": amdDigest, "mediaType": "application/octet-stream"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Scheme: "http"}

	layers, err := c.Layers(context.Background(), strings.TrimPrefix(srv.URL, "http://"), "acme/fw", "v1", nil)
	if err != nil {
		t.Fatalf("a plain manifest must still resolve: %v", err)
	}
	if len(layers) != 1 || layers[0].Digest != amdDigest {
		t.Errorf("layers = %+v", layers)
	}
}
