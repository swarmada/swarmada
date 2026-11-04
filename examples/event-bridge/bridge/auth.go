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

package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header names a caller signs a request with.
const (
	HeaderKeyID     = "X-Event-Bridge-Key-Id"
	HeaderTimestamp = "X-Event-Bridge-Timestamp"
	HeaderSignature = "X-Event-Bridge-Signature"

	signaturePrefix = "v1="
	// MinKeyBytes is the shortest key the bridge loads. A short HMAC key is a
	// guessable one; the bridge refuses to start with it rather than accept it.
	MinKeyBytes = 32
)

// SignedMaterial is what a signature covers: the key id, the timestamp and the body,
// joined by newlines. The timestamp is inside the signed material, so a caller cannot
// move a captured request's time forward without the key, and the key id is too, so
// one caller's signature cannot be presented under another caller's name.
func SignedMaterial(keyID, timestamp string, body []byte) []byte {
	m := make([]byte, 0, len(keyID)+len(timestamp)+len(body)+4)
	m = append(m, "v1\n"...)
	m = append(m, keyID...)
	m = append(m, '\n')
	m = append(m, timestamp...)
	m = append(m, '\n')
	return append(m, body...)
}

// Sign returns the signature header value for a request.
func Sign(key []byte, keyID, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(SignedMaterial(keyID, timestamp, body))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verifier authenticates requests: a known key id, a signed timestamp inside the
// window, a signature that matches, and a signature not already accepted.
//
// The replay cache is in memory and holds each accepted signature until its timestamp
// leaves the window; after that the timestamp check refuses the request on its own.
// A restart empties the cache. A request replayed inside the window after a restart
// is then authenticated again, and reaches the API server as the same SemanticEvent
// name, which already exists: it is answered as a duplicate and drafts nothing new.
type Verifier struct {
	keys    map[string][]byte
	window  time.Duration
	maxSeen int
	now     func() time.Time
	mu      sync.Mutex
	seen    map[string]time.Time
}

// NewVerifier builds a Verifier. window bounds how far a timestamp may be from now in
// either direction; maxSeen bounds the replay cache.
func NewVerifier(keys map[string][]byte, window time.Duration, maxSeen int, now func() time.Time) *Verifier {
	if now == nil {
		now = time.Now
	}
	return &Verifier{keys: keys, window: window, maxSeen: maxSeen, now: now, seen: map[string]time.Time{}}
}

// Verify authenticates one request and returns the caller's key id.
//
// The checks run cheapest-first and none of them reads the body as JSON: a request
// that is not authenticated is never parsed.
func (v *Verifier) Verify(keyID, timestamp, signature string, body []byte) (string, *Refusal) {
	if keyID == "" || timestamp == "" || signature == "" {
		return "", refuse(CodeMissingSignature, "headers %s, %s and %s are all required",
			HeaderKeyID, HeaderTimestamp, HeaderSignature)
	}
	key, ok := v.keys[keyID]
	if !ok {
		return "", refuse(CodeUnknownCaller, "no key is configured for key id %q", keyID)
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", refuse(CodeBadTimestamp, "%s must be a Unix time in seconds", HeaderTimestamp)
	}
	now := v.now()
	at := time.Unix(secs, 0)
	if d := now.Sub(at); d > v.window || d < -v.window {
		return "", refuse(CodeStaleTimestamp, "timestamp %s is %s from the bridge's clock; the window is ±%s",
			at.UTC().Format(time.RFC3339), d.Round(time.Second), v.window)
	}
	hexSig, ok := strings.CutPrefix(signature, signaturePrefix)
	if !ok {
		return "", refuse(CodeBadSignature, "signature must be %q followed by hex HMAC-SHA256", signaturePrefix)
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return "", refuse(CodeBadSignature, "signature is not hex")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(SignedMaterial(keyID, timestamp, body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return "", refuse(CodeBadSignature, "signature does not match key id %q, the timestamp and the body", keyID)
	}
	// Remembered only once it has verified, by its decoded value: no request without the key
	// takes a slot, and no re-encoding of an accepted one takes a second.
	return keyID, v.remember(keyID+"\n"+hex.EncodeToString(got), at, now)
}

// remember records an accepted signature until its timestamp leaves the window.
func (v *Verifier) remember(sig string, at, now time.Time) *Refusal {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.purge(now)
	if first, seen := v.seen[sig]; seen {
		return refuse(CodeReplayed, "this signed request was already accepted (signed at %s)",
			first.UTC().Format(time.RFC3339))
	}
	if len(v.seen) >= v.maxSeen {
		// Refuse rather than evict: an evicted signature would be replayable for
		// the rest of its window.
		return refuse(CodeReplayCacheFull, "the bridge holds %d signatures from the last %s and takes no more until they expire",
			len(v.seen), v.window)
	}
	v.seen[sig] = at
	return nil
}

// purge drops signatures whose timestamps have left the window; the timestamp check
// refuses those requests without the cache.
func (v *Verifier) purge(now time.Time) {
	for sig, at := range v.seen {
		if now.Sub(at) > v.window {
			delete(v.seen, sig)
		}
	}
}

func (v *Verifier) seenCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.purge(v.now())
	return len(v.seen)
}
