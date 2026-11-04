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
	"bytes"
	"strconv"
	"testing"
	"time"
)

var (
	doorKey  = bytes.Repeat([]byte("d"), MinKeyBytes)
	orderKey = bytes.Repeat([]byte("o"), MinKeyBytes)
	t0       = time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC)
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestVerifier(c *clock, maxSeen int) *Verifier {
	return NewVerifier(map[string][]byte{"door-controller": doorKey, "work-orders": orderKey},
		5*time.Minute, maxSeen, c.now)
}

func ts(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func wantCode(t *testing.T, got *Refusal, want Code) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("refused %s: %s, want accepted", got.Code, got.Reason)
		}
		return
	}
	if got == nil {
		t.Fatalf("accepted, want refusal %s", want)
	}
	if got.Code != want {
		t.Fatalf("refused %s (%s), want %s", got.Code, got.Reason, want)
	}
	if got.Reason == "" {
		t.Fatalf("refusal %s carries no reason", got.Code)
	}
}

func TestVerifyAcceptsAGoodSignature(t *testing.T) {
	c := &clock{t0}
	v := newTestVerifier(c, 10)
	body := []byte(`{"id":"a","type":"door.opened"}`)
	caller, r := v.Verify("door-controller", ts(t0), Sign(doorKey, "door-controller", ts(t0), body), body)
	wantCode(t, r, "")
	if caller != "door-controller" {
		t.Fatalf("caller %q", caller)
	}
}

func TestVerifyRefusals(t *testing.T) {
	body := []byte(`{"id":"a","type":"door.opened"}`)
	good := func(at time.Time) string { return Sign(doorKey, "door-controller", ts(at), body) }
	cases := []struct {
		name            string
		keyID, stamp    string
		signature       string
		body            []byte
		want            Code
		wantUnknownName bool
	}{
		{"no key id", "", ts(t0), good(t0), body, CodeMissingSignature, false},
		{"no timestamp", "door-controller", "", good(t0), body, CodeMissingSignature, false},
		{"no signature", "door-controller", ts(t0), "", body, CodeMissingSignature, false},
		{"unknown caller", "nobody", ts(t0), good(t0), body, CodeUnknownCaller, true},
		{"timestamp not a number", "door-controller", "yesterday", good(t0), body, CodeBadTimestamp, false},
		{"timestamp in milliseconds", "door-controller", strconv.FormatInt(t0.UnixMilli(), 10), good(t0), body, CodeStaleTimestamp, false},
		{"timestamp six minutes old", "door-controller", ts(t0.Add(-6 * time.Minute)), good(t0.Add(-6 * time.Minute)), body, CodeStaleTimestamp, false},
		{"timestamp six minutes ahead", "door-controller", ts(t0.Add(6 * time.Minute)), good(t0.Add(6 * time.Minute)), body, CodeStaleTimestamp, false},
		{"body changed after signing", "door-controller", ts(t0), good(t0), []byte(`{"id":"b","type":"door.opened"}`), CodeBadSignature, false},
		{"timestamp moved after signing", "door-controller", ts(t0.Add(time.Second)), good(t0), body, CodeBadSignature, false},
		{"another caller's key", "door-controller", ts(t0), Sign(orderKey, "door-controller", ts(t0), body), body, CodeBadSignature, false},
		{"another caller's signature under this name", "work-orders", ts(t0), good(t0), body, CodeBadSignature, false},
		{"not hex", "door-controller", ts(t0), "v1=zz", body, CodeBadSignature, false},
		{"no version prefix", "door-controller", ts(t0), good(t0)[3:], body, CodeBadSignature, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newTestVerifier(&clock{t0}, 10)
			_, r := v.Verify(tc.keyID, tc.stamp, tc.signature, tc.body)
			wantCode(t, r, tc.want)
		})
	}
}

func TestVerifyRefusesAReplayInsideTheWindow(t *testing.T) {
	c := &clock{t0}
	v := newTestVerifier(c, 10)
	body := []byte(`{"id":"a","type":"door.opened"}`)
	sig := Sign(doorKey, "door-controller", ts(t0), body)
	_, r := v.Verify("door-controller", ts(t0), sig, body)
	wantCode(t, r, "")
	c.t = t0.Add(2 * time.Minute)
	_, r = v.Verify("door-controller", ts(t0), sig, body)
	wantCode(t, r, CodeReplayed)
	// After the window the same request is refused by its timestamp alone, and the
	// cache has let the entry go.
	c.t = t0.Add(6 * time.Minute)
	_, r = v.Verify("door-controller", ts(t0), sig, body)
	wantCode(t, r, CodeStaleTimestamp)
	if n := v.seenCount(); n != 0 {
		t.Fatalf("replay cache holds %d entries after the window, want 0", n)
	}
}

func TestVerifyDoesNotRememberARefusedSignature(t *testing.T) {
	// A forged request must not be able to reserve a slot, or a caller who guessed
	// nothing could still block the real one.
	c := &clock{t0}
	v := newTestVerifier(c, 1)
	body := []byte(`{"id":"a","type":"door.opened"}`)
	_, r := v.Verify("door-controller", ts(t0), "v1="+string(bytes.Repeat([]byte("0"), 64)), body)
	wantCode(t, r, CodeBadSignature)
	_, r = v.Verify("door-controller", ts(t0), Sign(doorKey, "door-controller", ts(t0), body), body)
	wantCode(t, r, "")
}

func TestVerifyRefusesWhenTheReplayCacheIsFull(t *testing.T) {
	// Full means refuse, not evict: evicting the oldest entry would make that request
	// replayable for the rest of its window.
	c := &clock{t0}
	v := newTestVerifier(c, 1)
	first := []byte(`{"id":"a","type":"door.opened"}`)
	second := []byte(`{"id":"b","type":"door.opened"}`)
	firstSig := Sign(doorKey, "door-controller", ts(t0), first)
	_, r := v.Verify("door-controller", ts(t0), firstSig, first)
	wantCode(t, r, "")
	_, r = v.Verify("door-controller", ts(t0), Sign(doorKey, "door-controller", ts(t0), second), second)
	wantCode(t, r, CodeReplayCacheFull)
	_, r = v.Verify("door-controller", ts(t0), firstSig, first)
	wantCode(t, r, CodeReplayed)
}
