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

package artifact

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPFetcher_Success(t *testing.T) {
	want := []byte("detached-signature-bytes")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	}))
	defer ts.Close()

	f := &HTTPFetcher{Client: ts.Client()}
	got, err := f.Fetch(context.Background(), httpsURL(ts), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestHTTPFetcher_RejectsNonHTTPS(t *testing.T) {
	f := DefaultFetcher()
	for _, ref := range []string{"http://example.com/sig", "file:///etc/passwd", "oci://r/model:1"} {
		_, err := f.Fetch(context.Background(), ref, nil)
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("ref %q: err = %v, want ErrUnsupportedScheme", ref, err)
		}
	}
}

func TestHTTPFetcher_Non2xxIsError(t *testing.T) {
	for _, code := range []int{404, 500} {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		f := &HTTPFetcher{Client: ts.Client()}
		if _, err := f.Fetch(context.Background(), httpsURL(ts), nil); err == nil {
			t.Errorf("status %d: expected an error", code)
		}
		ts.Close()
	}
}

func TestHTTPFetcher_SizeCap(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("A"), 100))
	}))
	defer ts.Close()

	f := &HTTPFetcher{Client: ts.Client(), MaxBytes: 10}
	if _, err := f.Fetch(context.Background(), httpsURL(ts), nil); err == nil {
		t.Fatal("expected an over-cap error")
	}

	// Exactly at the cap succeeds.
	f2 := &HTTPFetcher{Client: ts.Client(), MaxBytes: 100}
	if _, err := f2.Fetch(context.Background(), httpsURL(ts), nil); err != nil {
		t.Fatalf("at-cap body should succeed: %v", err)
	}
}

func TestHTTPFetcher_BearerAndBasicAuth(t *testing.T) {
	var gotAuth string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()
	f := &HTTPFetcher{Client: ts.Client()}

	if _, err := f.Fetch(context.Background(), httpsURL(ts), &Credential{BearerToken: "tok123"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("bearer auth header = %q", gotAuth)
	}

	if _, err := f.Fetch(context.Background(), httpsURL(ts), &Credential{Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("basic auth header = %q, want Basic ...", gotAuth)
	}

	// Bearer wins when both are set.
	if _, err := f.Fetch(context.Background(), httpsURL(ts), &Credential{BearerToken: "t", Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer t" {
		t.Errorf("bearer should win over basic: %q", gotAuth)
	}

	// Nil credential sends no auth header.
	if _, err := f.Fetch(context.Background(), httpsURL(ts), nil); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("anonymous fetch sent auth header %q", gotAuth)
	}
}

// httpsURL returns the test server's URL (already https for a TLS server).
func httpsURL(ts *httptest.Server) string { return ts.URL }
