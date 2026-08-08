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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §9.1.7.3. The property under test is that a FORGED transparency-log response is refused.
//
// Index presence alone could never do this: the old check accepted any endpoint that returned a
// non-empty UUID array, so a hostile Rekor satisfied the dispatch gate with `["anything"]`. Each
// negative case below is a response that presence-only would have accepted.

const testArtifact = "sha256:" + "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// leafHash is the RFC-6962 leaf hash of an entry body.
func leafHash(body []byte) []byte {
	h := sha256.New()
	h.Write([]byte{leafHashPrefix})
	h.Write(body)
	return h.Sum(nil)
}

// entryBody embeds the artifact digest, the way a real Rekor entry references its subject.
func entryBody(artifact string) []byte {
	return []byte(`{"spec":{"data":{"hash":{"value":"` +
		strings.TrimPrefix(artifact, "sha256:") + `"}}}}`)
}

// signedEntry builds a self-consistent single-leaf entry: a one-node tree whose root IS the leaf
// hash, signed with key. A single-leaf tree keeps the proof empty, so the inclusion-proof and SET
// checks are exercised independently of Merkle-path arithmetic (which the multi-leaf case covers).
func signedEntry(t *testing.T, key *ecdsa.PrivateKey, artifact string) map[string]any {
	t.Helper()
	body := base64.StdEncoding.EncodeToString(entryBody(artifact))
	e := &logEntry{Body: body, IntegratedTime: 1700000000, LogID: "test-log", LogIndex: 0}
	e.Verification.InclusionProof.Hashes = nil
	e.Verification.InclusionProof.LogIndex = 0
	e.Verification.InclusionProof.TreeSize = 1
	e.Verification.InclusionProof.RootHash = hex.EncodeToString(leafHash(entryBody(artifact)))

	payload, err := canonicalSETPayload(e)
	if err != nil {
		t.Fatalf("canonical payload: %v", err)
	}
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return map[string]any{
		"body":           e.Body,
		"integratedTime": e.IntegratedTime,
		"logID":          e.LogID,
		"logIndex":       e.LogIndex,
		"verification": map[string]any{
			"signedEntryTimestamp": base64.StdEncoding.EncodeToString(sig),
			"inclusionProof": map[string]any{
				"hashes":   []string{},
				"logIndex": 0,
				"rootHash": e.Verification.InclusionProof.RootHash,
				"treeSize": 1,
			},
		},
	}
}

// rekorServer serves the index and entry APIs from a prepared entry.
func rekorServer(t *testing.T, uuids []string, entry map[string]any) (*Client, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/index/retrieve", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(uuids)
	})
	mux.HandleFunc("/api/v1/log/entries/", func(w http.ResponseWriter, r *http.Request) {
		if entry == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		uuid := strings.TrimPrefix(r.URL.Path, "/api/v1/log/entries/")
		_ = json.NewEncoder(w).Encode(map[string]any{uuid: entry})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client()}, srv.URL
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return k
}

// A genuine entry verifies, and the note says the proof and SET were checked.
func TestVerifyEntry_GenuineEntryVerifies(t *testing.T) {
	key := testKey(t)
	c, url := rekorServer(t, []string{"uuid-1"}, signedEntry(t, key, testArtifact))

	note, err := c.VerifyEntry(context.Background(), url, testArtifact, &key.PublicKey)
	if err != nil {
		t.Fatalf("a genuine entry must verify, got: %v", err)
	}
	if !strings.Contains(note, "inclusion proof") || !strings.Contains(note, "signed entry timestamp") {
		t.Errorf("note = %q, want it to state what was verified", note)
	}
}

// A SET signed by a DIFFERENT key is refused — the impersonation case.
func TestVerifyEntry_ForeignSignatureRefused(t *testing.T) {
	forger, pinned := testKey(t), testKey(t)
	c, url := rekorServer(t, []string{"uuid-1"}, signedEntry(t, forger, testArtifact))

	if _, err := c.VerifyEntry(context.Background(), url, testArtifact, &pinned.PublicKey); err == nil {
		t.Fatal("an entry signed by an unpinned key must be refused")
	}
}

// A tampered root hash breaks the inclusion proof even though the SET is well-formed.
func TestVerifyEntry_TamperedRootRefused(t *testing.T) {
	key := testKey(t)
	entry := signedEntry(t, key, testArtifact)
	ip := entry["verification"].(map[string]any)["inclusionProof"].(map[string]any)
	ip["rootHash"] = hex.EncodeToString(make([]byte, sha256.Size)) // all zeroes

	c, url := rekorServer(t, []string{"uuid-1"}, entry)
	if _, err := c.VerifyEntry(context.Background(), url, testArtifact, &key.PublicKey); err == nil {
		t.Fatal("a tampered rootHash must fail the inclusion proof")
	}
}

// An entry that is genuine but describes ANOTHER artifact is refused. Presence-only never checked
// this: the returned entry need not have related to the artifact being dispatched.
func TestVerifyEntry_EntryForAnotherArtifactRefused(t *testing.T) {
	key := testKey(t)
	other := "sha256:" + strings.Repeat("11", 32)
	c, url := rekorServer(t, []string{"uuid-1"}, signedEntry(t, key, other))

	_, err := c.VerifyEntry(context.Background(), url, testArtifact, &key.PublicKey)
	if err == nil {
		t.Fatal("an entry for a different artifact must be refused")
	}
	if !strings.Contains(err.Error(), "does not reference artifact digest") {
		t.Errorf("error = %q, want it to name the artifact mismatch", err.Error())
	}
}

// A missing SET is refused rather than treated as "nothing to check".
func TestVerifyEntry_MissingSETRefused(t *testing.T) {
	key := testKey(t)
	entry := signedEntry(t, key, testArtifact)
	entry["verification"].(map[string]any)["signedEntryTimestamp"] = ""

	c, url := rekorServer(t, []string{"uuid-1"}, entry)
	if _, err := c.VerifyEntry(context.Background(), url, testArtifact, &key.PublicKey); err == nil {
		t.Fatal("an entry with no signedEntryTimestamp must be refused")
	}
}

// An empty index means the artifact is not logged.
func TestVerifyEntry_NoEntryRefused(t *testing.T) {
	key := testKey(t)
	c, url := rekorServer(t, []string{}, nil)
	if _, err := c.VerifyEntry(context.Background(), url, testArtifact, &key.PublicKey); err == nil {
		t.Fatal("an artifact with no log entry must be refused")
	}
}

// With NO key pinned the check degrades to presence — and must say so, so an operator can tell a
// verified entry from an unverified one.
func TestVerifyEntry_NoPinnedKeyDegradesAudibly(t *testing.T) {
	c, url := rekorServer(t, []string{"uuid-1"}, nil)

	note, err := c.VerifyEntry(context.Background(), url, testArtifact, nil)
	if err != nil {
		t.Fatalf("presence-only mode must still succeed when the hash is indexed, got: %v", err)
	}
	if !strings.Contains(note, "NOT verified") {
		t.Errorf("note = %q, want it to state plainly that the entry was not cryptographically verified", note)
	}
}

// Even in the degraded mode, an absent entry still refuses.
func TestVerifyEntry_NoPinnedKeyStillRefusesAbsent(t *testing.T) {
	c, url := rekorServer(t, []string{}, nil)
	if _, err := c.VerifyEntry(context.Background(), url, testArtifact, nil); err == nil {
		t.Fatal("an absent entry must be refused even without a pinned key")
	}
}

// A multi-leaf inclusion proof verifies, exercising the Merkle path rather than the trivial
// single-leaf case.
func TestVerifyInclusionProof_MultiLeafPath(t *testing.T) {
	body := entryBody(testArtifact)
	sibling := sha256.Sum256([]byte("sibling-leaf"))

	// Two-leaf tree, our entry at index 0: root = H(0x01 || leaf0 || leaf1).
	n := sha256.New()
	n.Write([]byte{nodeHashPrefix})
	n.Write(leafHash(body))
	n.Write(sibling[:])
	root := n.Sum(nil)

	e := &logEntry{Body: base64.StdEncoding.EncodeToString(body)}
	e.Verification.InclusionProof.Hashes = []string{hex.EncodeToString(sibling[:])}
	e.Verification.InclusionProof.LogIndex = 0
	e.Verification.InclusionProof.TreeSize = 2
	e.Verification.InclusionProof.RootHash = hex.EncodeToString(root)

	if err := verifyInclusionProof(e); err != nil {
		t.Fatalf("a valid two-leaf proof must verify: %v", err)
	}

	// Corrupting the sibling breaks it.
	e.Verification.InclusionProof.Hashes = []string{hex.EncodeToString(make([]byte, sha256.Size))}
	if err := verifyInclusionProof(e); err == nil {
		t.Error("a corrupted sibling hash must fail the proof")
	}
}

// An out-of-range index is rejected before any hashing.
func TestVerifyInclusionProof_RangeChecked(t *testing.T) {
	e := &logEntry{Body: base64.StdEncoding.EncodeToString(entryBody(testArtifact))}
	e.Verification.InclusionProof.TreeSize = 4
	e.Verification.InclusionProof.LogIndex = 9
	e.Verification.InclusionProof.RootHash = hex.EncodeToString(make([]byte, sha256.Size))
	if err := verifyInclusionProof(e); err == nil {
		t.Fatal("logIndex outside treeSize must be refused")
	}
}
