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

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// fakeEstopAuthz records the estop-verb authorization calls and returns a canned
// decision, standing in for a real SubjectAccessReview.
type fakeEstopAuthz struct {
	allow   bool
	err     error
	calls   int
	gotVerb string
	gotUser string
}

func (f *fakeEstopAuthz) Authorize(_ context.Context, user authenticationv1.UserInfo, verb string, _ schema.GroupResource, _, _ string) (bool, error) {
	f.calls++
	f.gotVerb = verb
	f.gotUser = user.Username
	return f.allow, f.err
}

// estopZone builds a FleetZone with (or without) the estop-triggered annotation. An
// empty triggerVal leaves the annotation absent.
func estopZone(name, triggerVal string) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name}}
	if triggerVal != "" {
		z.Annotations = map[string]string{estopTriggeredAnnotation: triggerVal}
	}
	return z
}

// ctxAsUser injects an admission request carrying the given username, so
// RequestFromContext inside the validator resolves a UserInfo.
func ctxAsUser(username string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: username},
		},
	})
}

func newZoneValidatorWithAuthz(authz VerbAuthorizer) *FleetZoneValidator {
	return &FleetZoneValidator{EstopAuthz: authz}
}

// Setting the estop-triggered annotation requires the estop-trigger verb; an
// authorized user is admitted.
func TestFleetZoneEstop_TriggerAllowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := newZoneValidatorWithAuthz(authz)

	_, err := v.ValidateUpdate(ctxAsUser("alice"),
		estopZone("dock", ""), estopZone("dock", "2026-07-09T00:00:00Z"))
	if err != nil {
		t.Fatalf("authorized estop-trigger should be admitted, got: %v", err)
	}
	if authz.gotVerb != estopTriggerVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, estopTriggerVerb)
	}
	if authz.gotUser != "alice" {
		t.Errorf("user = %q, want alice", authz.gotUser)
	}
}

// An unauthorized estop-trigger is rejected fail-closed (Forbidden).
func TestFleetZoneEstop_TriggerDenied(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: false})

	_, err := v.ValidateUpdate(ctxAsUser("mallory"),
		estopZone("dock", ""), estopZone("dock", "fire"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("unauthorized estop-trigger must be Forbidden, got: %v", err)
	}
}

// Re-valuing the annotation (a re-trigger) also requires estop-trigger.
func TestFleetZoneEstop_RetriggerRequiresTrigger(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := newZoneValidatorWithAuthz(authz)

	if _, err := v.ValidateUpdate(ctxAsUser("op"),
		estopZone("dock", "v1"), estopZone("dock", "v2")); err != nil {
		t.Fatalf("re-trigger should be admitted for an authorized user: %v", err)
	}
	if authz.gotVerb != estopTriggerVerb {
		t.Errorf("re-trigger verb = %q, want %q", authz.gotVerb, estopTriggerVerb)
	}
}

// Removing the annotation is an estop-clear and requires the (stricter) clear verb.
func TestFleetZoneEstop_ClearRequiresClearVerb(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := newZoneValidatorWithAuthz(authz)

	if _, err := v.ValidateUpdate(ctxAsUser("admin"),
		estopZone("dock", "fire"), estopZone("dock", "")); err != nil {
		t.Fatalf("authorized estop-clear should be admitted: %v", err)
	}
	if authz.gotVerb != estopClearVerb {
		t.Errorf("clear verb = %q, want %q", authz.gotVerb, estopClearVerb)
	}
}

// A non-estop update (annotation unchanged) never triggers an authorization call.
func TestFleetZoneEstop_NonEstopUpdateSkipsAuthz(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny IF consulted
	v := newZoneValidatorWithAuthz(authz)

	if _, err := v.ValidateUpdate(ctxAsUser("anyone"),
		estopZone("dock", "fire"), estopZone("dock", "fire")); err != nil {
		t.Fatalf("an unchanged estop annotation must not require authz: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times on a non-estop update, want 0", authz.calls)
	}
}

// A nil authorizer fails closed on an estop change — enforcement is never bypassed.
func TestFleetZoneEstop_NilAuthorizerFailsClosed(t *testing.T) {
	v := &FleetZoneValidator{} // no EstopAuthz

	_, err := v.ValidateUpdate(ctxAsUser("op"),
		estopZone("dock", ""), estopZone("dock", "fire"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("nil authorizer must fail closed (Forbidden) on an estop change, got: %v", err)
	}
}

// No admission request in context (so no user identity) fails closed on an estop change.
func TestFleetZoneEstop_NoRequestFailsClosed(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: true})

	_, err := v.ValidateUpdate(context.Background(),
		estopZone("dock", ""), estopZone("dock", "fire"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a missing admission request must fail closed on an estop change, got: %v", err)
	}
}

// A SubjectAccessReview error fails closed (never admit on an undecidable check).
func TestFleetZoneEstop_AuthzErrorFailsClosed(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{err: errors.New("apiserver down")})

	_, err := v.ValidateUpdate(ctxAsUser("op"),
		estopZone("dock", ""), estopZone("dock", "fire"))
	if err == nil {
		t.Fatal("an authorization error must not admit the estop change")
	}
}
