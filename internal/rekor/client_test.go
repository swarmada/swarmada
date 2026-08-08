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

package rekor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func tlsClient(ts *httptest.Server) *Client { return &Client{HTTP: ts.Client()} }

func TestHasEntry_Present(t *testing.T) {
	var gotBody string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/index/retrieve" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`["uuid-1","uuid-2"]`))
	}))
	defer ts.Close()

	ok, err := tlsClient(ts).HasEntry(context.Background(), ts.URL, "sha256:abc")
	if err != nil {
		t.Fatalf("HasEntry: %v", err)
	}
	if !ok {
		t.Error("expected present=true for a non-empty UUID list")
	}
	if !strings.Contains(gotBody, `"hash":"sha256:abc"`) {
		t.Errorf("request body = %q, want the hash", gotBody)
	}
}

func TestHasEntry_Absent(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	ok, err := tlsClient(ts).HasEntry(context.Background(), ts.URL, "sha256:abc")
	if err != nil {
		t.Fatalf("HasEntry: %v", err)
	}
	if ok {
		t.Error("expected present=false for an empty UUID list")
	}
}

func TestHasEntry_Non2xxIsError(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	if _, err := tlsClient(ts).HasEntry(context.Background(), ts.URL, "sha256:abc"); err == nil {
		t.Fatal("expected an error on a 500 from rekor")
	}
}

func TestHasEntry_RejectsNonHTTPS(t *testing.T) {
	if _, err := New().HasEntry(context.Background(), "http://rekor.example", "sha256:abc"); err == nil {
		t.Fatal("a non-https rekorUrl must be rejected")
	}
}
