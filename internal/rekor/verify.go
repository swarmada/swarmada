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
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Cryptographic verification of a Rekor entry (RFC-0001 §9.1.7.3).
//
// WHY THIS EXISTS. The index-presence check (HasEntry) proves only that the endpoint returned a
// non-empty UUID array for a hash. It does not prove an entry exists in the log, that the entry
// covers the artifact being dispatched, or that the log ever attested it. A hostile or impersonated
// Rekor satisfies presence with `["anything"]` — and the firmware controller treats the result as a
// fail-closed dispatch gate, so that answer would let an unlogged artifact through.
//
// VerifyEntry closes that by checking two independent things against a PINNED log key:
//
//   - the INCLUSION PROOF: recomputing the RFC-6962 Merkle path from the entry's leaf to the
//     claimed root, so the entry provably sits in a tree with that root; and
//   - the SIGNED ENTRY TIMESTAMP: the log's own signature over the entry, so the root and entry
//     come from the log operator rather than from whoever answered the request.
//
// The key must be pinned by the operator. Fetching it from the endpoint under verification would be
// circular — a forged server would serve the key matching its own forged signature.

// leafHashPrefix and nodeHashPrefix are the RFC-6962 domain separators. They exist so a leaf hash
// can never be confused with an interior node hash, which is what stops a second-preimage attack
// that presents an interior node as a leaf.
const (
	leafHashPrefix byte = 0x00
	nodeHashPrefix byte = 0x01
)

// logEntry is the subset of Rekor's GET /api/v1/log/entries/{uuid} response this verifies.
type logEntry struct {
	Body           string `json:"body"`
	IntegratedTime int64  `json:"integratedTime"`
	LogID          string `json:"logID"`
	LogIndex       int64  `json:"logIndex"`
	Verification   struct {
		SignedEntryTimestamp string `json:"signedEntryTimestamp"`
		InclusionProof       struct {
			Hashes   []string `json:"hashes"`
			LogIndex int64    `json:"logIndex"`
			RootHash string   `json:"rootHash"`
			TreeSize int64    `json:"treeSize"`
		} `json:"inclusionProof"`
	} `json:"verification"`
}

// VerifyEntry proves that hash has a genuine, logged entry at rekorURL.
//
// With logKey nil the check DEGRADES to index presence and says so in the returned note — a caller
// must be able to tell a verified entry from an unverified one, so the weaker mode is never silent.
// With logKey set, every failure is an error: fail closed.
func (c *Client) VerifyEntry(ctx context.Context, rekorURL, hash string, logKey crypto.PublicKey) (string, error) {
	uuids, err := c.entryUUIDs(ctx, rekorURL, hash)
	if err != nil {
		return "", err
	}
	if len(uuids) == 0 {
		return "", fmt.Errorf("artifact %s has no entry in the %s transparency log", hash, rekorURL)
	}
	if logKey == nil {
		return fmt.Sprintf("index presence only (%d entr(y/ies)); no rekorPublicKey pinned, so the "+
			"entry's inclusion proof and signed-entry-timestamp were NOT verified", len(uuids)), nil
	}

	// Any one genuine entry proves the artifact is logged; report the first that verifies fully and
	// the reason the last one failed otherwise, so a misconfiguration is diagnosable.
	var lastErr error
	for _, uuid := range uuids {
		entry, err := c.fetchEntry(ctx, rekorURL, uuid)
		if err != nil {
			lastErr = err
			continue
		}
		if err := verifyEntry(entry, hash, logKey); err != nil {
			lastErr = fmt.Errorf("entry %s: %w", uuid, err)
			continue
		}
		return fmt.Sprintf("verified inclusion proof + signed entry timestamp (entry %s, log index %d)",
			uuid, entry.LogIndex), nil
	}
	return "", fmt.Errorf("no verifiable transparency-log entry for %s: %w", hash, lastErr)
}

// verifyEntry runs the three checks that make an entry trustworthy.
func verifyEntry(e *logEntry, hash string, logKey crypto.PublicKey) error {
	// 1. The entry must cover THIS artifact. Without this the proof and signature could be genuine
	//    but belong to some other artifact the log happens to hold — index presence alone never
	//    established that the returned entry relates to what is being dispatched.
	if err := entryCoversArtifact(e, hash); err != nil {
		return err
	}
	// 2. The entry sits in a tree with the claimed root.
	if err := verifyInclusionProof(e); err != nil {
		return err
	}
	// 3. The log signed it. This is what binds the root to the log operator rather than to the
	//    responding server.
	return verifySET(e, logKey)
}

// entryCoversArtifact confirms the canonicalised artifact digest appears in the entry body.
func entryCoversArtifact(e *logEntry, hash string) error {
	raw, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		return fmt.Errorf("entry body is not valid base64: %w", err)
	}
	want := strings.ToLower(strings.TrimPrefix(hash, "sha256:"))
	if want == "" || !strings.Contains(strings.ToLower(string(raw)), want) {
		return fmt.Errorf("entry does not reference artifact digest %s", hash)
	}
	return nil
}

// verifyInclusionProof recomputes the RFC-6962 Merkle path from the entry's leaf to the claimed
// root. A tampered root, a tampered sibling hash, or a proof of the wrong length all fail here.
func verifyInclusionProof(e *logEntry) error {
	p := e.Verification.InclusionProof
	if p.TreeSize <= 0 || p.LogIndex < 0 || p.LogIndex >= p.TreeSize {
		return fmt.Errorf("inclusion proof has an out-of-range index %d for tree size %d", p.LogIndex, p.TreeSize)
	}
	root, err := hex.DecodeString(p.RootHash)
	if err != nil || len(root) != sha256.Size {
		return fmt.Errorf("inclusion proof rootHash %q is not a sha256 hex digest", p.RootHash)
	}
	leafBytes, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		return fmt.Errorf("entry body is not valid base64: %w", err)
	}

	// RFC-6962 leaf hash: SHA256(0x00 || entry).
	h := sha256.New()
	h.Write([]byte{leafHashPrefix})
	h.Write(leafBytes)
	computed := h.Sum(nil)

	index, size := p.LogIndex, p.TreeSize
	for _, sibHex := range p.Hashes {
		sib, err := hex.DecodeString(sibHex)
		if err != nil || len(sib) != sha256.Size {
			return fmt.Errorf("inclusion proof contains a malformed sibling hash %q", sibHex)
		}
		if size == 0 {
			return fmt.Errorf("inclusion proof is longer than the tree depth allows")
		}
		n := sha256.New()
		n.Write([]byte{nodeHashPrefix})
		if index%2 == 1 || index+1 == size {
			// Right child (or the last node at this level): sibling is on the left.
			n.Write(sib)
			n.Write(computed)
			for index%2 == 0 {
				index /= 2
				size /= 2
			}
		} else {
			n.Write(computed)
			n.Write(sib)
		}
		computed = n.Sum(nil)
		index /= 2
		size /= 2
	}
	if !bytes.Equal(computed, root) {
		return fmt.Errorf("inclusion proof does not reach the claimed root (computed %x, claimed %s)",
			computed, p.RootHash)
	}
	return nil
}

// verifySET checks the log's signature over the entry.
//
// The signed payload is the canonicalised JSON of {body, integratedTime, logID, logIndex} with keys
// in lexicographic order and no whitespace — Rekor's own SET construction. Marshalling a Go map
// gives exactly that ordering, which is why the payload is built from one rather than a struct.
func verifySET(e *logEntry, logKey crypto.PublicKey) error {
	if e.Verification.SignedEntryTimestamp == "" {
		return fmt.Errorf("entry carries no signedEntryTimestamp")
	}
	sig, err := base64.StdEncoding.DecodeString(e.Verification.SignedEntryTimestamp)
	if err != nil {
		return fmt.Errorf("signedEntryTimestamp is not valid base64: %w", err)
	}
	payload, err := canonicalSETPayload(e)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)

	switch k := logKey.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			return fmt.Errorf("signedEntryTimestamp does not verify against the pinned log key")
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(k, payload, sig) {
			return fmt.Errorf("signedEntryTimestamp does not verify against the pinned log key")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig); err != nil {
			return fmt.Errorf("signedEntryTimestamp does not verify against the pinned log key: %w", err)
		}
	default:
		return fmt.Errorf("unsupported rekorPublicKey type %T", logKey)
	}
	return nil
}

func canonicalSETPayload(e *logEntry) ([]byte, error) {
	fields := map[string]any{
		"body":           e.Body,
		"integratedTime": e.IntegratedTime,
		"logID":          e.LogID,
		"logIndex":       e.LogIndex,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(fields[k])
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// fetchEntry retrieves one entry by UUID.
func (c *Client) fetchEntry(ctx context.Context, rekorURL, uuid string) (*logEntry, error) {
	base, err := url.Parse(strings.TrimRight(rekorURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing rekorUrl %q: %w", rekorURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base.String()+"/api/v1/log/entries/"+url.PathEscape(uuid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching rekor entry %s: %w", uuid, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rekor entry %s: unexpected status %d", uuid, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > defaultMaxBytes {
		return nil, fmt.Errorf("rekor entry %s: response exceeds the %d-byte cap", uuid, defaultMaxBytes)
	}
	// The response is a map keyed by UUID.
	var byUUID map[string]logEntry
	if err := json.Unmarshal(raw, &byUUID); err != nil {
		return nil, fmt.Errorf("parsing rekor entry %s: %w", uuid, err)
	}
	for _, e := range byUUID {
		entry := e
		return &entry, nil
	}
	return nil, fmt.Errorf("rekor entry %s: empty response", uuid)
}
