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
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// cancelAction builds a FleetAction with (or without) the cancel-requested annotation.
func cancelAction(name, cancelVal string) *fleetv1.FleetAction {
	tk := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name}}
	if cancelVal != "" {
		tk.Annotations = map[string]string{cancelRequestedAnnotation: cancelVal}
	}
	return tk
}

func newActionValidatorWithAuthz(authz VerbAuthorizer) *FleetActionValidator {
	return &FleetActionValidator{CancelAuthz: authz}
}

// Adding the cancel-requested annotation requires the `cancel` verb; an authorized
// user is admitted, and the SAR is for the cancel verb.
func TestFleetActionCancel_Allowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := newActionValidatorWithAuthz(authz)

	if _, err := v.ValidateUpdate(ctxAsUser("op"), cancelAction("t1", ""), cancelAction("t1", "true")); err != nil {
		t.Fatalf("authorized cancel should be admitted, got: %v", err)
	}
	if authz.gotVerb != cancelVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, cancelVerb)
	}
}

// An unauthorized cancel is rejected fail-closed (Forbidden).
func TestFleetActionCancel_Denied(t *testing.T) {
	v := newActionValidatorWithAuthz(&fakeEstopAuthz{allow: false})
	_, err := v.ValidateUpdate(ctxAsUser("mallory"), cancelAction("t1", ""), cancelAction("t1", "true"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("unauthorized cancel must be Forbidden, got: %v", err)
	}
}

// A action created already cancel-requested is gated on the cancel verb.
func TestFleetActionCancel_OnCreateDenied(t *testing.T) {
	v := newActionValidatorWithAuthz(&fakeEstopAuthz{allow: false})
	_, err := v.ValidateCreate(ctxAsUser("mallory"), cancelAction("t1", "true"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("unauthorized create-with-cancel must be Forbidden, got: %v", err)
	}
}

// A non-cancel update (annotation unchanged) never consults authz.
func TestFleetActionCancel_NonCancelUpdateSkipsAuthz(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny IF consulted
	v := newActionValidatorWithAuthz(authz)

	// Already-cancel-requested on both sides (re-value of the reason) is not a NEW
	// cancel and must not require the verb again.
	if _, err := v.ValidateUpdate(ctxAsUser("anyone"), cancelAction("t1", "reason-a"), cancelAction("t1", "reason-b")); err != nil {
		t.Fatalf("re-valuing an existing cancel must not require authz: %v", err)
	}
	// A plain update with no cancel annotation at all.
	if _, err := v.ValidateUpdate(ctxAsUser("anyone"), cancelAction("t1", ""), cancelAction("t1", "")); err != nil {
		t.Fatalf("a non-cancel update must not require authz: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times on non-cancel updates, want 0", authz.calls)
	}
}

// A nil authorizer fails closed on a cancel request.
func TestFleetActionCancel_NilAuthorizerFailsClosed(t *testing.T) {
	v := &FleetActionValidator{}
	_, err := v.ValidateUpdate(ctxAsUser("op"), cancelAction("t1", ""), cancelAction("t1", "true"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("nil authorizer must fail closed (Forbidden) on cancel, got: %v", err)
	}
}

// No admission request in context fails closed on a cancel request.
func TestFleetActionCancel_NoRequestFailsClosed(t *testing.T) {
	v := newActionValidatorWithAuthz(&fakeEstopAuthz{allow: true})
	_, err := v.ValidateUpdate(context.Background(), cancelAction("t1", ""), cancelAction("t1", "true"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a missing admission request must fail closed on cancel, got: %v", err)
	}
}

// A SubjectAccessReview error fails closed (never admit on an undecidable check).
func TestFleetActionCancel_AuthzErrorFailsClosed(t *testing.T) {
	v := newActionValidatorWithAuthz(&fakeEstopAuthz{err: errors.New("apiserver down")})
	_, err := v.ValidateUpdate(ctxAsUser("op"), cancelAction("t1", ""), cancelAction("t1", "true"))
	if err == nil {
		t.Fatal("an authorization error must not admit the cancel")
	}
}
