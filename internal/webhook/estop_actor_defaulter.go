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
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Carrying the authenticated operator identity onto estop and rollout-resume writes
// (ADR-0046).
//
// THE PROBLEM. The admitting webhook authorizes a REAL user — authorizeEstopVerb runs a
// SubjectAccessReview against req.UserInfo — and then discards that identity. The estop
// controllers reconcile asynchronously, long after the admission request is gone, so they
// stamped a synthetic actor derived from the scope (`zone-estop:<zone>`) onto both
// ESTOP_TRIGGERED and ESTOP_CLEARED. The safety audit log could not attribute an emergency
// stop to a person, which is the one thing a safety case built on it needs.
//
// THE SHAPE. A mutating webhook stamps swarmada.io/estop-actor from req.UserInfo.Username
// at admission; the controllers read it off the object they already fetch. The identity is
// persisted at the only moment it exists, by the only component that sees it.
//
// WHY NOT A SELF-ASSERTED ANNOTATION. The alternative — require the caller to set the
// annotation and have the validator refuse a mismatch — cannot be reconciled with the rule
// that an estop is never blocked by identity plumbing. A kubectl client cannot reliably
// compute the username the API server will derive for it (client cert, token, exec plugin,
// OIDC), so the honest version needs a SelfSubjectReview round-trip on the estop path, and
// any failure of that round-trip REFUSES the emergency stop. See ADR-0046 Alternatives.
//
// FAIL-OPEN, DELIBERATELY. Every function here returns without error when the identity
// cannot be resolved. The mutators are registered failurePolicy=Ignore for the same reason:
// attribution is best-effort, authorization is not. Authorization keeps failing closed in
// the validating webhooks, which are unchanged. A missing stamp degrades the audit entry to
// an explicitly unattributed actor (see internal/controller: estopActor); it never costs a
// safe stop.

const (
	// AnnEstopActor carries the authenticated username that performed an estop
	// trigger/clear or a rollout resume. Written ONLY by the mutating webhooks in this
	// package, from the admission request's UserInfo — never by a client. A value a
	// client supplies is overwritten on the same admission, so it cannot be spoofed by
	// writing the annotation directly.
	AnnEstopActor = "swarmada.io/estop-actor"

	// annRolloutResume mirrors the controller-side constant (internal/controller's
	// rolloutResumeAnnotation). It is duplicated rather than imported because
	// internal/controller depends on this package's siblings, not the reverse; a
	// consistency test pins the two spellings together.
	annRolloutResume = "swarmada.io/rollout-resume"
)

// admissionUser returns the authenticated username on the request, or "" when there is no
// admission request in context or it carries no username. Never an error: the caller's
// contract is to skip stamping, not to fail the write.
func admissionUser(ctx context.Context) string {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return ""
	}
	return req.UserInfo.Username
}

// oldAnnotations decodes the pre-update object's annotations from the admission request.
// Returns (nil, false) on CREATE, on a missing request, or on an undecodable old object —
// each of which the callers treat as "no previous value".
func oldAnnotations(ctx context.Context) (map[string]string, bool) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || len(req.OldObject.Raw) == 0 {
		return nil, false
	}
	// Only metadata is needed, so decode the envelope rather than the concrete type. This
	// keeps one helper working for all five carrier kinds.
	var meta struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(req.OldObject.Raw, &meta); err != nil {
		return nil, false
	}
	return meta.Metadata.Annotations, true
}

// transitioned reports whether the value of key changed between the old and new
// annotations — which covers all three estop edges: added (trigger), re-valued (re-fire),
// and removed (clear). A write that leaves the key untouched is not an estop transition and
// must not restamp the actor, or an unrelated update would silently reattribute the
// standing estop to whoever made it.
func transitioned(ctx context.Context, newAnns map[string]string, key string) bool {
	old, hadOld := oldAnnotations(ctx)
	if !hadOld {
		// CREATE: a transition only if the key is present on the incoming object.
		_, present := newAnns[key]
		return present
	}
	oldVal, oldPresent := old[key]
	newVal, newPresent := newAnns[key]
	return oldPresent != newPresent || oldVal != newVal
}

// stampActorOn writes the authenticated username onto obj's estop-actor annotation when
// key transitioned on this request. It returns without stamping when no transition
// occurred or no identity is resolvable.
//
// On a transition WITH an identity the annotation is overwritten unconditionally, so a
// client-supplied value never survives: the stamp is the webhook's assertion about who
// made this request, not a field the caller may fill in.
//
// On a transition WITHOUT an identity any stale value is REMOVED rather than left in
// place. Leaving it would attribute this operator's estop to the previous one — a wrong
// name in a safety audit log is worse than an absent one.
func stampActorOn(ctx context.Context, obj metav1.Object, key string) {
	anns := obj.GetAnnotations()
	if !transitioned(ctx, anns, key) {
		return
	}
	user := admissionUser(ctx)
	if user == "" {
		if _, stale := anns[AnnEstopActor]; stale {
			delete(anns, AnnEstopActor)
			obj.SetAnnotations(anns)
		}
		return
	}
	if anns == nil {
		anns = map[string]string{}
	}
	anns[AnnEstopActor] = user
	obj.SetAnnotations(anns)
}

// stampEstopActor stamps on an estop-triggered transition (trigger, re-fire, or clear).
func stampEstopActor(ctx context.Context, obj metav1.Object) {
	stampActorOn(ctx, obj, estopTriggeredAnnotation)
}

// stampResumeActor stamps on a rollout-resume transition (ADR-0041's operator intake).
func stampResumeActor(ctx context.Context, obj metav1.Object) {
	stampActorOn(ctx, obj, annRolloutResume)
}
