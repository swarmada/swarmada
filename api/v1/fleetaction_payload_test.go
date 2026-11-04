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

package v1

import (
	"bytes"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// A payload written as plain keys is the payload. Before ActionPayload decoded the whole
// object, every key other than `raw` was discarded at decode, so a typed read returned an
// empty payload and every controller copy wrote `payload: {}`.
func TestActionPayload_PlainKeysSurviveDecode(t *testing.T) {
	in := `{"route":{"corridorHalfWidthM":0.9,"frame":"map","waypoints":[{"x":1,"y":2}]}}`
	var p ActionPayload
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !jsonEqual(t, p.Raw, []byte(in)) {
		t.Fatalf("Raw = %s, want the whole object %s", p.Raw, in)
	}
	out, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !jsonEqual(t, out, []byte(in)) {
		t.Errorf("re-encoded payload = %s, want %s", out, in)
	}
}

// The RFC-0001 spelling, an object whose only key is `raw` holding base64, keeps its
// meaning: Raw is the decoded document, and it re-encodes in the form it was written in.
func TestActionPayload_RawFormKeepsItsMeaning(t *testing.T) {
	in := `{"raw":"eyJ4IjoxLjV9"}` // {"x":1.5}
	var p ActionPayload
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(p.Raw) != `{"x":1.5}` {
		t.Fatalf("Raw = %q, want the decoded document {\"x\":1.5}", p.Raw)
	}
	out, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != in {
		t.Errorf("re-encoded payload = %s, want the form it was written in, %s", out, in)
	}
}

// Only an object whose ONLY key is a base64 `raw` string is read as the raw form. Any
// other object, including one with `raw` beside other keys, is the payload itself.
func TestActionPayload_OnlyASoleBase64RawIsTheRawForm(t *testing.T) {
	for _, in := range []string{
		`{"raw":"eyJ4IjoxLjV9","x":1}`, // raw with a sibling
		`{"raw":5}`,                    // raw that is not a string
		`{"raw":"not base64!"}`,        // raw that is not base64
		`{"raw":{"x":1}}`,              // raw that is an object
		`{}`,
	} {
		var p ActionPayload
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatalf("unmarshal %s: %v", in, err)
		}
		if !jsonEqual(t, p.Raw, []byte(in)) {
			t.Errorf("%s: Raw = %s, want the whole object", in, p.Raw)
		}
	}
}

// A payload that is not a JSON object is refused at decode. The schema already refuses it
// at the API server; a Go decode must not invent a meaning for it.
func TestActionPayload_NonObjectIsRefused(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"s"`, `3`, `true`} {
		var p ActionPayload
		if err := json.Unmarshal([]byte(in), &p); err == nil {
			t.Errorf("%s decoded without error (Raw = %s); want it refused", in, p.Raw)
		}
	}
}

// Bytes set in Go encode as the object they are when they are one, and as the raw form
// when they are not (binary, or a JSON value that is not an object), so nothing a caller
// can put in Raw is lost or changes meaning on the way through the API server.
func TestActionPayload_GoBytesRoundTrip(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"bin":"A-14"}`),
		{0x00, 0xff, 0x10},               // binary
		[]byte(`[1,2,3]`),                // JSON, not an object
		[]byte(`{"raw":"eyJ4IjoxfQ=="}`), // an object that would read back as the raw form
	} {
		in := ActionPayload{Raw: raw}
		b, err := json.Marshal(&in)
		if err != nil {
			t.Fatalf("marshal %q: %v", raw, err)
		}
		var out ActionPayload
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if !bytes.Equal(out.Raw, raw) {
			t.Errorf("Raw %q encoded as %s and decoded as %q", raw, b, out.Raw)
		}
	}
}

// An empty payload that is present encodes as {}, so `has(self.payload)` stays true for
// it in CEL (the executor rule refuses a payload that is set at all).
func TestActionPayload_EmptyEncodesAsEmptyObject(t *testing.T) {
	b, err := json.Marshal(&ActionPayload{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{}` {
		t.Errorf("empty payload encoded as %s, want {}", b)
	}
}

// The FleetTask member copy is a DeepCopy, and unstructured conversion is how several
// clients read typed objects. Both must keep the payload and the form it was written in.
func TestActionPayload_DeepCopyAndUnstructuredKeepTheForm(t *testing.T) {
	for _, in := range []string{`{"raw":"eyJ4IjoxLjV9"}`, `{"route":{"corridorHalfWidthM":0.9}}`} {
		spec := FleetActionSpec{Type: ActionTypePatrol, Payload: &ActionPayload{}}
		if err := json.Unmarshal([]byte(in), spec.Payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		cp := spec.DeepCopy()
		b, err := json.Marshal(cp.Payload)
		if err != nil {
			t.Fatalf("marshal copy: %v", err)
		}
		if !jsonEqual(t, b, []byte(in)) {
			t.Errorf("DeepCopy re-encoded %s as %s", in, b)
		}

		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
		if err != nil {
			t.Fatalf("to unstructured: %v", err)
		}
		var back FleetActionSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u, &back); err != nil {
			t.Fatalf("from unstructured: %v", err)
		}
		if back.Payload == nil || !bytes.Equal(back.Payload.Raw, spec.Payload.Raw) {
			t.Errorf("%s: unstructured round trip gave %+v, want Raw %s", in, back.Payload, spec.Payload.Raw)
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("not JSON: %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("not JSON: %s: %v", b, err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
