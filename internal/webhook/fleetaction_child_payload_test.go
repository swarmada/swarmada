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

package webhook

import (
	"encoding/json"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A child with a payload: ActionPayload has an unexported field, on which Semantic.DeepEqual
// panics. A panic fails the webhook call, so with failurePolicy Fail a write that left the payload
// as it was stayed refused, the controller's included.
func TestFleetActionChild_APayloadIsCompared(t *testing.T) {
	task := taskRef("dock", "1", true)
	withPayload := func(raw string) *fleetv1.FleetAction {
		a := childAction(task)
		a.Spec.Payload = &fleetv1.ActionPayload{Raw: []byte(raw)}
		return a
	}
	written := func(form string) *fleetv1.FleetAction {
		a := childAction(task)
		a.Spec.Payload = &fleetv1.ActionPayload{}
		if err := json.Unmarshal([]byte(form), a.Spec.Payload); err != nil {
			t.Fatal(err)
		}
		return a
	}
	cancelled := func(raw string) *fleetv1.FleetAction {
		a := withPayload(raw)
		a.Annotations = map[string]string{cancelRequestedAnnotation: "stop this"}
		return a
	}
	cases := []struct {
		name        string
		user        string
		allow       bool
		old, new    *fleetv1.FleetAction
		wantRefused bool
	}{
		{"nothing-changes", "alice", true, withPayload(`{"x":1}`), withPayload(`{"x":1}`), false},
		{"an-operator-cancels", "alice", true, withPayload(`{"x":1}`), cancelled(`{"x":1}`), false},
		{"cancel-without-the-verb", "alice", false, withPayload(`{"x":1}`), cancelled(`{"x":1}`), true},
		{"a-user-changes-the-payload", "alice", true, withPayload(`{"x":1}`), withPayload(`{"x":2}`), true},
		{"a-user-removes-the-payload", "alice", true, withPayload(`{"x":1}`), childAction(task), true},
		{"a-user-adds-a-payload", "alice", true, childAction(task), withPayload(`{"x":1}`), true},
		{"a-user-changes-the-priority-beside-a-payload", "alice", true, withPayload(`{"x":1}`),
			withPriority(withPayload(`{"x":1}`), fleetv1.ActionPriorityHigh), true},
		{"a-user-unwraps-the-raw-form", "alice", true, written(`{"raw":"eyJ4IjoxfQ=="}`), written(`{"x":1}`), true},
		{"a-user-wraps-a-payload-in-the-raw-form", "alice", true, written(`{"x":1}`), written(`{"raw":"eyJ4IjoxfQ=="}`), true},
		{"an-empty-raw-form-is-not-an-empty-object", "alice", true, written(`{"raw":""}`), written(`{}`), true},
		{"the-raw-form-kept", "alice", true, written(`{"raw":"eyJ4IjoxfQ=="}`), written(`{"raw":"eyJ4IjoxfQ=="}`), false},
		{"the-controller-writes-a-child", controllerUser, false, withPayload(`{"x":1}`), withPayload(`{"x":1}`), false},
		{"the-controller-changes-a-payload", controllerUser, false, withPayload(`{"x":1}`), withPayload(`{"x":2}`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := &FleetActionValidator{CancelAuthz: &fakeEstopAuthz{allow: c.allow}}
			err := v.guardFleetTaskChild(ctxAsUser(c.user), c.old, c.new)
			switch {
			case c.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !c.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
		})
	}
}

// samePayload discards MarshalJSON's error because there is none. If that ever stops being true,
// two failed encodings would compare equal and the guard would admit a change, so the claim is held
// here over every path through MarshalJSON: no bytes, an object, a value that is not one, binary,
// and the raw form as read and as an ambiguous object written in Go.
func TestActionPayload_MarshalJSONCannotFail(t *testing.T) {
	read := func(form string) fleetv1.ActionPayload {
		var p fleetv1.ActionPayload
		if err := json.Unmarshal([]byte(form), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, p := range map[string]fleetv1.ActionPayload{
		"no-bytes":           {},
		"empty-bytes":        {Raw: []byte{}},
		"an-object":          {Raw: []byte(`{"x":1}`)},
		"not-json":           {Raw: []byte(`{"x":`)},
		"not-an-object":      {Raw: []byte(`[1,2]`)},
		"binary":             {Raw: []byte{0xff, 0x00, 0xfe}},
		"ambiguous-raw":      {Raw: []byte(`{"raw":"eA=="}`)},
		"the-raw-form":       read(`{"raw":"eyJ4IjoxfQ=="}`),
		"the-empty-raw-form": read(`{"raw":""}`),
		"read-as-an-object":  read(`{"x":1}`),
	} {
		if b, err := p.MarshalJSON(); err != nil || len(b) == 0 {
			t.Errorf("%s: MarshalJSON gave %q, %v", name, b, err)
		}
	}
}
