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
	"encoding/base64"
	"encoding/json"
	"errors"
)

// rawFormPayload is the RFC-0001 spelling of a payload: its JSON bytes, base64-encoded
// under a single `raw` key.
type rawFormPayload struct {
	Raw []byte `json:"raw"`
}

// errPayloadNotObject refuses a payload that is not a JSON object. The CRD schema
// declares spec.payload an object, so the API server never stores one.
var errPayloadNotObject = errors.New("spec.payload must be a JSON object")

// UnmarshalJSON reads a payload object. An object whose only key is `raw`, holding a
// base64 string, is the RFC-0001 spelling and decodes to that string's bytes; any other
// object is itself the payload and is kept whole.
func (p *ActionPayload) UnmarshalJSON(data []byte) error {
	if raw, ok, err := rawFormOf(data); err != nil {
		return err
	} else if ok {
		p.Raw, p.rawForm = raw, true
		return nil
	}
	p.Raw, p.rawForm = append([]byte(nil), data...), false
	return nil
}

// MarshalJSON writes a payload back in the form it was read in. Bytes set in Go are
// written as the object they are when they are one, and in the raw form otherwise
// (binary, a JSON value that is not an object, or an object that would read back as the
// raw form), so no value of Raw is lost or changes meaning through the API server.
func (p ActionPayload) MarshalJSON() ([]byte, error) {
	if len(p.Raw) == 0 && !p.rawForm {
		return []byte(`{}`), nil
	}
	if !p.rawForm && isJSONObject(p.Raw) {
		if _, ambiguous, err := rawFormOf(p.Raw); err == nil && !ambiguous {
			return p.Raw, nil
		}
	}
	return json.Marshal(rawFormPayload{Raw: p.Raw})
}

// rawFormOf reports whether data is an object in the raw form and, if so, the decoded
// bytes. It fails only when data is not a JSON object.
func rawFormOf(data []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, false, errPayloadNotObject
	}
	enc, ok := fields["raw"]
	if !ok || len(fields) != 1 {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(enc, &s); err != nil {
		return nil, false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false, nil
	}
	return raw, true, nil
}

func isJSONObject(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{' && json.Valid(b)
}
