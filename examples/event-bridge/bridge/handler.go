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
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// MaxBodyBytes bounds a request body. An envelope is a handful of short strings.
const MaxBodyBytes = 16 << 10

// Creator is the one Kubernetes call the bridge makes. A controller-runtime client
// satisfies it; the bridge's Role grants create on semanticevents and nothing else.
type Creator interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
}

// Response is the JSON body of every answer.
type Response struct {
	// Result is created, duplicate or refused.
	Result        string   `json:"result"`
	SemanticEvent string   `json:"semanticEvent,omitempty"`
	Source        string   `json:"source,omitempty"`
	Type          string   `json:"type,omitempty"`
	Refusal       *Refusal `json:"refusal,omitempty"`
}

// Handler serves POST /v1/events.
type Handler struct {
	Verifier  *Verifier
	Mapper    *Mapper
	Creator   Creator
	Namespace string
	Now       func() time.Time
	Log       *slog.Logger
}

// ServeHTTP authenticates, maps and creates, in that order. Nothing is parsed before
// the signature verifies, and nothing reaches the API server before the envelope maps.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.answer(w, r, http.StatusMethodNotAllowed, Response{Result: "refused",
			Refusal: refuse(CodeMalformed, "only POST is served")}, "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		h.refused(w, r, refuse(CodeTooLarge, "body is over %d bytes", MaxBodyBytes), "")
		return
	}
	caller, ref := h.Verifier.Verify(r.Header.Get(HeaderKeyID), r.Header.Get(HeaderTimestamp),
		r.Header.Get(HeaderSignature), body)
	if ref != nil {
		h.refused(w, r, ref, "")
		return
	}
	env, ref := ParseEnvelope(body)
	if ref != nil {
		h.refused(w, r, ref, caller)
		return
	}
	ev, ref := h.Mapper.Map(caller, env, h.Now())
	if ref != nil {
		h.refused(w, r, ref, caller)
		return
	}

	obj := &fleetv1.SemanticEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ev.Name,
			Namespace: h.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":             "event-bridge",
				"event-bridge.examples.swarmada.io/caller": caller,
			},
		},
		Spec: fleetv1.SemanticEventSpec{Source: ev.Source, Type: ev.Type, Payload: ev.Payload},
	}
	if ev.OccurredAt != nil {
		t := metav1.NewTime(*ev.OccurredAt)
		obj.Spec.OccurredAt = &t
	}
	resp := Response{SemanticEvent: ev.Name, Source: ev.Source, Type: ev.Type}
	err = h.Creator.Create(r.Context(), obj)
	switch {
	case err == nil:
		resp.Result = "created"
		h.answer(w, r, http.StatusCreated, resp, caller)
	case apierrors.IsAlreadyExists(err):
		// The same caller's same id: one occurrence, delivered again. It already
		// has its SemanticEvent, and that event already drafted what it drafts.
		resp.Result = "duplicate"
		h.answer(w, r, http.StatusOK, resp, caller)
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		h.refused(w, r, refuse(CodeAPIRefused, "the API server refused SemanticEvent %s: %v", ev.Name, err), caller)
	default:
		h.refused(w, r, refuse(CodeAPIUnavailable, "creating SemanticEvent %s: %v", ev.Name, err), caller)
	}
}

func (h *Handler) refused(w http.ResponseWriter, r *http.Request, ref *Refusal, caller string) {
	h.answer(w, r, ref.HTTPStatus(), Response{Result: "refused", Refusal: ref}, caller)
}

// answer writes the response and logs one line for it. Neither ever carries the
// signature or a key.
func (h *Handler) answer(w http.ResponseWriter, r *http.Request, status int, resp Response, caller string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		h.Log.Warn("writing response", "err", err)
	}
	attrs := []any{"status", status, "result", resp.Result, "remote", r.RemoteAddr,
		"keyId", r.Header.Get(HeaderKeyID), "caller", caller}
	if resp.SemanticEvent != "" {
		attrs = append(attrs, "semanticEvent", resp.SemanticEvent, "source", resp.Source, "type", resp.Type)
	}
	if resp.Refusal != nil {
		attrs = append(attrs, "code", resp.Refusal.Code, "reason", resp.Refusal.Reason)
	}
	h.Log.Info("request", attrs...)
}
