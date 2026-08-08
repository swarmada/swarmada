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

// ociMock is a tiny OCI Distribution registry: one repo, one tag, one config blob.
func ociMock(t *testing.T, labels string) *httptest.Server {
	t.Helper()
	const repo = "models/item-recognition"
	const configDigest = "sha256:cfg000"
	const manifestDigest = "sha256:manifest111"
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/"+repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"` + repo + `","tags":["1.0.0","2.3.0","2.1.0"]}`))
	})
	mux.HandleFunc("/v2/"+repo+"/manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write([]byte(`{"config":{"digest":"` + configDigest + `"}}`))
	})
	mux.HandleFunc("/v2/"+repo+"/blobs/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"config":{"Labels":{"swarmada.metrics":` + labels + `}}}`))
	})
	return httptest.NewServer(mux)
}

// hostOf returns a test server's host:port and a client pinned to it (http).
func hostOf(ts *httptest.Server) (string, *Client) {
	host := strings.TrimPrefix(ts.URL, "http://")
	return host, &Client{HTTP: ts.Client(), Scheme: "http"}
}

func TestClient_ListTags(t *testing.T) {
	ts := ociMock(t, `"{}"`)
	defer ts.Close()
	host, c := hostOf(ts)

	tags, err := c.ListTags(context.Background(), host, "models/item-recognition", nil)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 3 || Highest(tags) != "2.3.0" {
		t.Fatalf("tags = %v (highest %q)", tags, Highest(tags))
	}
}

func TestClient_DescriptorAndConfigLabels(t *testing.T) {
	ts := ociMock(t, `"{\"pickSuccessRate\":0.94}"`)
	defer ts.Close()
	host, c := hostOf(ts)

	md, cd, err := c.Descriptor(context.Background(), host, "models/item-recognition", "2.3.0", nil)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if md != "sha256:manifest111" || cd != "sha256:cfg000" {
		t.Fatalf("manifestDigest=%q configDigest=%q", md, cd)
	}
	labels, err := c.ConfigLabels(context.Background(), host, "models/item-recognition", cd, nil)
	if err != nil {
		t.Fatalf("ConfigLabels: %v", err)
	}
	if labels["swarmada.metrics"] == "" {
		t.Fatalf("labels = %v", labels)
	}
}

func TestClient_Blob(t *testing.T) {
	want := []byte("detached-signature-bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/sha256:abc") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(want)
	}))
	defer ts.Close()
	host, c := hostOf(ts)

	got, err := c.Blob(context.Background(), host, "models/item", "sha256:abc", nil)
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("blob = %q, want %q", got, want)
	}
}

func TestClient_Layers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/manifests/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"layers":[{"digest":"sha256:aaa","mediaType":"application/octet-stream"}]}`))
	}))
	defer ts.Close()
	host, c := hostOf(ts)

	layers, err := c.Layers(context.Background(), host, "models/item", "sig-tag", nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	if len(layers) != 1 || layers[0].Digest != "sha256:aaa" || layers[0].MediaType != "application/octet-stream" {
		t.Fatalf("layers = %+v", layers)
	}
}

func TestClient_Non2xxIsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	host, c := hostOf(ts)
	if _, err := c.ListTags(context.Background(), host, "missing/repo", nil); err == nil {
		t.Fatal("expected an error on 404")
	}
}

func TestClient_CredentialApplied(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"tags":[]}`))
	}))
	defer ts.Close()
	host, c := hostOf(ts)

	_, _ = c.ListTags(context.Background(), host, "r", &Credential{BearerToken: "tok"})
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q, want Bearer tok", gotAuth)
	}
}

func TestScheme_LocalhostIsHTTP(t *testing.T) {
	c := &Client{}
	if got := c.scheme("localhost:5000"); got != "http" {
		t.Errorf("localhost scheme = %q, want http", got)
	}
	if got := c.scheme("ghcr.io"); got != "https" {
		t.Errorf("remote scheme = %q, want https", got)
	}
}

func TestHighestAndNewer(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"1.0.0", "1.10.0", "1.9.0"}, "1.10.0"}, // numeric, not lexical
		{[]string{"v2.0.0", "v2.0.1"}, "v2.0.1"},         // leading v
		{[]string{"1.2", "1.2.1"}, "1.2.1"},              // missing component sorts lower
		{nil, ""},
		{[]string{""}, ""},
	}
	for _, tc := range cases {
		if got := Highest(tc.tags); got != tc.want {
			t.Errorf("Highest(%v) = %q, want %q", tc.tags, got, tc.want)
		}
	}
	if !Newer("2.0.0", "1.9.9") {
		t.Error("2.0.0 should be newer than 1.9.9")
	}
	if Newer("1.0.0", "1.0.0") {
		t.Error("equal versions are not newer")
	}
	if Newer("1.0.0", "2.0.0") {
		t.Error("1.0.0 is not newer than 2.0.0")
	}
}
