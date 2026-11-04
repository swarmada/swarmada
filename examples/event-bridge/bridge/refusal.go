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

// Package bridge turns an authenticated external event into a SemanticEvent, or into
// a named refusal. It holds no Kubernetes credentials of its own: the caller hands it
// something that can create SemanticEvents, and that is all it ever asks of it.
package bridge

import (
	"fmt"
	"net/http"
)

// Code names why a request was refused. Every refusal carries one, so a caller can
// act on the reason without parsing prose.
type Code string

// Refusal codes. The authentication codes come first: nothing about a request is
// read, beyond its headers and size, until its signature has been checked.
const (
	// CodeMissingSignature: a signature, key id or timestamp header is absent.
	CodeMissingSignature Code = "MissingSignature"
	// CodeUnknownCaller: the key id names no key this bridge holds.
	CodeUnknownCaller Code = "UnknownCaller"
	// CodeBadTimestamp: the timestamp header is not a Unix time in seconds.
	CodeBadTimestamp Code = "BadTimestamp"
	// CodeStaleTimestamp: the signed timestamp is outside the replay window.
	CodeStaleTimestamp Code = "StaleTimestamp"
	// CodeBadSignature: the signature does not match the key, timestamp and body.
	CodeBadSignature Code = "BadSignature"
	// CodeReplayed: this exact signed request was already accepted inside the window.
	CodeReplayed Code = "Replayed"
	// CodeReplayCacheFull: the bridge cannot remember one more signature, so it
	// refuses rather than forget one it has seen.
	CodeReplayCacheFull Code = "ReplayCacheFull"

	// CodeMalformed: the body is not exactly one JSON envelope with known fields.
	CodeMalformed Code = "Malformed"
	// CodeTooLarge: the body is over the size limit.
	CodeTooLarge Code = "TooLarge"
	// CodeUnknownType: the envelope's type is not in the bridge's vocabulary.
	CodeUnknownType Code = "UnknownType"
	// CodeNotPermitted: the caller is authenticated but not bound to this type.
	CodeNotPermitted Code = "NotPermitted"
	// CodeUnknownField: a data key the type does not declare.
	CodeUnknownField Code = "UnknownField"
	// CodeMissingField: a data key the type requires is absent or empty.
	CodeMissingField Code = "MissingField"
	// CodeBadValue: a value is too long, has characters outside its set, or is out
	// of range.
	CodeBadValue Code = "BadValue"
	// CodeUndeclaredZone: the event names a zone the bridge was not configured with.
	CodeUndeclaredZone Code = "UndeclaredZone"
	// CodeUndeclaredSchedule: the event names a schedule the bridge was not
	// configured with.
	CodeUndeclaredSchedule Code = "UndeclaredSchedule"
	// CodeUnsupportedWorkOrder: a work order for a task the bridge does not map.
	CodeUnsupportedWorkOrder Code = "UnsupportedWorkOrder"
	// CodeBadOccurredAt: occurredAt is not RFC 3339, is in the future, or is older
	// than the bridge accepts.
	CodeBadOccurredAt Code = "BadOccurredAt"

	// CodeAPIRefused: the Kubernetes API server refused the SemanticEvent.
	CodeAPIRefused Code = "APIRefused"
	// CodeAPIUnavailable: the Kubernetes API server could not be reached.
	CodeAPIUnavailable Code = "APIUnavailable"
)

// Refusal is a request the bridge did not turn into a SemanticEvent, and why.
type Refusal struct {
	Code   Code   `json:"code"`
	Reason string `json:"reason"`
}

func (r *Refusal) Error() string { return fmt.Sprintf("%s: %s", r.Code, r.Reason) }

func refuse(code Code, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// HTTPStatus is the status code a refusal is answered with.
func (r *Refusal) HTTPStatus() int {
	switch r.Code {
	case CodeMissingSignature, CodeUnknownCaller, CodeBadTimestamp, CodeStaleTimestamp,
		CodeBadSignature, CodeReplayed:
		return http.StatusUnauthorized
	case CodeNotPermitted:
		return http.StatusForbidden
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeMalformed:
		return http.StatusBadRequest
	case CodeAPIRefused:
		return http.StatusBadGateway
	case CodeReplayCacheFull, CodeAPIUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}
