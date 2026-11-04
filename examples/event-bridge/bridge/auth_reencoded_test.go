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
	"net/http"
	"strings"
	"testing"
	"time"
)

// A signature is its value, not its spelling: every hex spelling of one accepted signature is
// the same signed request, and the bridge accepts it once within the window.

// reencoded spells sig's hex in upper case (all), or with the case of its first letter flipped.
func reencoded(sig string, all bool) string {
	hexSig := strings.TrimPrefix(sig, signaturePrefix)
	if all {
		return signaturePrefix + strings.ToUpper(hexSig)
	}
	b := []byte(hexSig)
	for i, ch := range b {
		if ch >= 'a' && ch <= 'f' {
			b[i] = ch - 'a' + 'A'
			break
		}
	}
	return signaturePrefix + string(b)
}

func TestVerifyAReEncodedSignatureIsAReplay(t *testing.T) {
	for _, tc := range []struct {
		name string
		all  bool
	}{{"upper-case", true}, {"one-letter-flipped", false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t0}
			v := newTestVerifier(c, 10)
			body := []byte(`{"id":"a","type":"door.opened"}`)
			sig := Sign(doorKey, "door-controller", ts(t0), body)
			again := reencoded(sig, tc.all)
			if again == sig {
				t.Fatalf("the test signature has no letter to re-spell: %s", sig)
			}
			_, r := v.Verify("door-controller", ts(t0), sig, body)
			wantCode(t, r, "")
			c.t = t0.Add(time.Minute)
			_, r = v.Verify("door-controller", ts(t0), again, body)
			wantCode(t, r, CodeReplayed)
		})
	}
}

func TestVerifyReEncodingsOfOneSignatureHoldOneSlot(t *testing.T) {
	// Spellings of one accepted signature take no slot of their own, so they cannot fill the
	// cache and turn away the next caller.
	c := &clock{t0}
	v := newTestVerifier(c, 2)
	first := []byte(`{"id":"a","type":"door.opened"}`)
	second := []byte(`{"id":"b","type":"door.opened"}`)
	sig := Sign(doorKey, "door-controller", ts(t0), first)
	_, r := v.Verify("door-controller", ts(t0), sig, first)
	wantCode(t, r, "")
	for _, all := range []bool{true, false} {
		_, r = v.Verify("door-controller", ts(t0), reencoded(sig, all), first)
		wantCode(t, r, CodeReplayed)
	}
	_, r = v.Verify("work-orders", ts(t0), Sign(orderKey, "work-orders", ts(t0), second), second)
	wantCode(t, r, "")
	if n := v.seenCount(); n != 2 {
		t.Fatalf("replay cache holds %d entries for two accepted requests, want 2", n)
	}
}

func TestHandlerAReEncodedResendIsRefusedAfterItsEventIsGone(t *testing.T) {
	// The event a request created is deleted inside the window (a person withdraws its draft).
	// A resend of the same signed request, re-spelled, is a replay: it creates nothing.
	c := &clock{t0}
	f := &fakeCreator{}
	h := newTestHandler(t, c, f)
	sig := Sign(doorKey, "door-controller", ts(t0), doorBody)
	if code, resp := post(t, h, "door-controller", ts(t0), sig, doorBody); code != http.StatusCreated {
		t.Fatalf("first: %d %+v", code, resp)
	}
	f.names = map[string]bool{} // the event is gone
	c.t = t0.Add(time.Minute)
	code, resp := post(t, h, "door-controller", ts(t0), reencoded(sig, true), doorBody)
	if code != http.StatusUnauthorized || resp.Refusal == nil || resp.Refusal.Code != CodeReplayed {
		t.Fatalf("re-spelled resend: %d %+v, want 401 %s", code, resp, CodeReplayed)
	}
	if len(f.created) != 1 {
		t.Fatalf("the re-spelled resend created the event again: %d creates", len(f.created))
	}
}
