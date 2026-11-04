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
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// fakeCreator stands in for the API server: it remembers names, answers AlreadyExists
// for a name it has, and can be told to fail.
type fakeCreator struct {
	created []*fleetv1.SemanticEvent
	names   map[string]bool
	err     error
}

func (f *fakeCreator) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	if f.err != nil {
		return f.err
	}
	ev, ok := obj.(*fleetv1.SemanticEvent)
	if !ok {
		return errors.New("the bridge created something other than a SemanticEvent")
	}
	if f.names[ev.Name] {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "swarmada.io", Resource: "semanticevents"}, ev.Name)
	}
	if f.names == nil {
		f.names = map[string]bool{}
	}
	f.names[ev.Name] = true
	f.created = append(f.created, ev.DeepCopy())
	return nil
}

func newTestHandler(t *testing.T, c *clock, f *fakeCreator) *Handler {
	t.Helper()
	return &Handler{
		Verifier:  newTestVerifier(c, 100),
		Mapper:    mustMapper(t),
		Creator:   f,
		Namespace: "warehouse-a",
		Now:       c.now,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func post(t *testing.T, h http.Handler, keyID, stamp, sig string, body []byte) (int, Response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if keyID != "" {
		req.Header.Set(HeaderKeyID, keyID)
	}
	if stamp != "" {
		req.Header.Set(HeaderTimestamp, stamp)
	}
	if sig != "" {
		req.Header.Set(HeaderSignature, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("status %d, body not JSON: %q", rec.Code, rec.Body.String())
	}
	return rec.Code, resp
}

func signed(t *testing.T, h http.Handler, key []byte, keyID string, at time.Time, body []byte) (int, Response) {
	t.Helper()
	return post(t, h, keyID, ts(at), Sign(key, keyID, ts(at), body), body)
}

var doorBody = []byte(`{"id":"door-7-1","type":"door.opened","data":{"door":"north-7"}}`)

func TestHandlerCreatesOneSemanticEvent(t *testing.T) {
	c := &clock{t0}
	f := &fakeCreator{}
	code, resp := signed(t, newTestHandler(t, c, f), doorKey, "door-controller", t0, doorBody)
	if code != http.StatusCreated || resp.Result != "created" {
		t.Fatalf("%d %+v", code, resp)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d objects", len(f.created))
	}
	ev := f.created[0]
	if ev.Namespace != "warehouse-a" || ev.Name != EventName("door-controller", "door-7-1") ||
		ev.Spec.Source != "building-doors" || ev.Spec.Type != "door.opened.after-hours" {
		t.Fatalf("event %s/%s source=%s type=%s", ev.Namespace, ev.Name, ev.Spec.Source, ev.Spec.Type)
	}
	if ev.Labels["app.kubernetes.io/managed-by"] != "event-bridge" || ev.Labels["event-bridge.examples.swarmada.io/caller"] != "door-controller" {
		t.Fatalf("labels %v", ev.Labels)
	}
	if resp.SemanticEvent != ev.Name || resp.Type != ev.Spec.Type || resp.Source != ev.Spec.Source {
		t.Fatalf("response does not name what it created: %+v", resp)
	}
}

func TestHandlerSameEventTwiceIsOneSemanticEvent(t *testing.T) {
	// The door controller delivers again with a fresh timestamp and signature: that
	// is not a replay, it is the same occurrence, and it must draft nothing new.
	c := &clock{t0}
	f := &fakeCreator{}
	h := newTestHandler(t, c, f)
	code, first := signed(t, h, doorKey, "door-controller", t0, doorBody)
	if code != http.StatusCreated {
		t.Fatalf("first: %d %+v", code, first)
	}
	c.t = t0.Add(30 * time.Second)
	code, second := signed(t, h, doorKey, "door-controller", c.t, doorBody)
	if code != http.StatusOK || second.Result != "duplicate" || second.SemanticEvent != first.SemanticEvent {
		t.Fatalf("second: %d %+v, want 200 duplicate of %s", code, second, first.SemanticEvent)
	}
	if len(f.created) != 1 {
		t.Fatalf("%d SemanticEvents for one occurrence", len(f.created))
	}
}

func TestHandlerRefusesAReplayBeforeItReachesTheAPI(t *testing.T) {
	c := &clock{t0}
	f := &fakeCreator{}
	h := newTestHandler(t, c, f)
	sig := Sign(doorKey, "door-controller", ts(t0), doorBody)
	if code, resp := post(t, h, "door-controller", ts(t0), sig, doorBody); code != http.StatusCreated {
		t.Fatalf("first: %d %+v", code, resp)
	}
	f.names = map[string]bool{} // a replay must be refused by the bridge, not by the API
	code, resp := post(t, h, "door-controller", ts(t0), sig, doorBody)
	if code != http.StatusUnauthorized || resp.Refusal == nil || resp.Refusal.Code != CodeReplayed {
		t.Fatalf("replay: %d %+v", code, resp)
	}
	if len(f.created) != 1 {
		t.Fatalf("the replay reached the API: %d creates", len(f.created))
	}
}

func TestHandlerRefusals(t *testing.T) {
	cases := []struct {
		name       string
		keyID      string
		key        []byte
		body       []byte
		badSig     bool
		wantStatus int
		wantCode   Code
	}{
		{"unknown caller", "nobody", doorKey, doorBody, false, http.StatusUnauthorized, CodeUnknownCaller},
		// A bad signature is refused before the body is read: a malformed body
		// under a bad signature is BadSignature, not Malformed.
		{"bad signature on a malformed body", "door-controller", doorKey, []byte(`{not json`), true, http.StatusUnauthorized, CodeBadSignature},
		{"signed by the wrong key", "door-controller", orderKey, doorBody, false, http.StatusUnauthorized, CodeBadSignature},
		{"malformed body, good signature", "door-controller", doorKey, []byte(`{"id":"a"`), false, http.StatusBadRequest, CodeMalformed},
		{"caller not bound to the type", "door-controller", doorKey,
			[]byte(`{"id":"w","type":"workorder.created","data":{"workOrder":"1","zone":"loading-bay","task":"clean"}}`), false,
			http.StatusForbidden, CodeNotPermitted},
		{"type outside the vocabulary", "door-controller", doorKey, []byte(`{"id":"a","type":"door.closed"}`), false,
			http.StatusUnprocessableEntity, CodeUnknownType},
		{"too large", "door-controller", doorKey, bytes.Repeat([]byte(" "), MaxBodyBytes+1), false,
			http.StatusRequestEntityTooLarge, CodeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCreator{}
			h := newTestHandler(t, &clock{t0}, f)
			sig := Sign(tc.key, tc.keyID, ts(t0), tc.body)
			if tc.badSig {
				sig = "v1=" + string(bytes.Repeat([]byte("0"), 64))
			}
			code, resp := post(t, h, tc.keyID, ts(t0), sig, tc.body)
			if code != tc.wantStatus || resp.Result != "refused" || resp.Refusal == nil || resp.Refusal.Code != tc.wantCode {
				t.Fatalf("%d %+v, want %d %s", code, resp, tc.wantStatus, tc.wantCode)
			}
			if len(f.created) != 0 {
				t.Fatalf("a refused request created %d objects", len(f.created))
			}
		})
	}
}

func TestHandlerOnlyPost(t *testing.T) {
	h := newTestHandler(t, &clock{t0}, &fakeCreator{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET answered %d", rec.Code)
	}
}

func TestHandlerNamesTheAPIServersRefusal(t *testing.T) {
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantCode   Code
	}{
		{apierrors.NewForbidden(schema.GroupResource{Group: "swarmada.io", Resource: "semanticevents"}, "x", errors.New("policy")),
			http.StatusBadGateway, CodeAPIRefused},
		{errors.New("connection refused"), http.StatusServiceUnavailable, CodeAPIUnavailable},
	} {
		f := &fakeCreator{err: tc.err}
		code, resp := signed(t, newTestHandler(t, &clock{t0}, f), doorKey, "door-controller", t0, doorBody)
		if code != tc.wantStatus || resp.Refusal == nil || resp.Refusal.Code != tc.wantCode {
			t.Fatalf("%v: %d %+v", tc.err, code, resp)
		}
	}
}
