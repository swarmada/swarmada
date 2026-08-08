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
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func ftAct(name string) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{Name: name, Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
}

func ftWith(name string, actions ...fleetv1.FleetTaskAction) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.FleetTaskSpec{Actions: actions},
	}
}

func newFTValidator(a VerbAuthorizer) *FleetTaskValidator { return &FleetTaskValidator{AppendAuthz: a} }

// Appending a new action requires the `append` verb; an authorized user is admitted.
func TestFleetTaskAppend_Allowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := newFTValidator(authz)
	if _, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a")), ftWith("t1", ftAct("a"), ftAct("b"))); err != nil {
		t.Fatalf("authorized append should be admitted, got: %v", err)
	}
	if authz.gotVerb != appendVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, appendVerb)
	}
}

// An unauthorized append is rejected fail-closed (Forbidden).
func TestFleetTaskAppend_Denied(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: false})
	_, err := v.ValidateUpdate(ctxAsUser("mallory"), ftWith("t1", ftAct("a")), ftWith("t1", ftAct("a"), ftAct("b")))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("unauthorized append must be Forbidden, got: %v", err)
	}
}

// Editing an existing action is Forbidden regardless of the append grant (immutability).
func TestFleetTaskAppend_EditExistingForbidden(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	edited := ftAct("a")
	edited.DependsOn = []string{"x"}
	_, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a")), ftWith("t1", edited))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("editing an existing action must be Forbidden, got: %v", err)
	}
}

// Removing an action is Forbidden (append-only).
func TestFleetTaskAppend_RemoveForbidden(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	_, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a"), ftAct("b")), ftWith("t1", ftAct("a")))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("removing an action must be Forbidden, got: %v", err)
	}
}

// A no-op update (no append) never consults authz.
func TestFleetTaskAppend_NoChangeSkipsAuthz(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny IF consulted
	v := newFTValidator(authz)
	if _, err := v.ValidateUpdate(ctxAsUser("anyone"), ftWith("t1", ftAct("a")), ftWith("t1", ftAct("a"))); err != nil {
		t.Fatalf("a non-append update must not require authz: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times on a non-append update, want 0", authz.calls)
	}
}

// Changing an immutable policy is Forbidden.
func TestFleetTaskAppend_PolicyImmutable(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	newT := ftWith("t1", ftAct("a"))
	newT.Spec.CompletionPolicy = fleetv1.CompletionPolicyAny
	_, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a")), newT)
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("changing completionPolicy must be Forbidden, got: %v", err)
	}
}

// A nil authorizer fails closed on an append.
func TestFleetTaskAppend_NilAuthorizerFailsClosed(t *testing.T) {
	v := &FleetTaskValidator{}
	_, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a")), ftWith("t1", ftAct("a"), ftAct("b")))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("nil authorizer must fail closed (Forbidden) on append, got: %v", err)
	}
}

// A SubjectAccessReview error fails closed (never admit on an undecidable check).
func TestFleetTaskAppend_AuthzErrorFailsClosed(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{err: errors.New("apiserver down")})
	if _, err := v.ValidateUpdate(ctxAsUser("op"), ftWith("t1", ftAct("a")), ftWith("t1", ftAct("a"), ftAct("b"))); err == nil {
		t.Fatal("an authorization error must not admit the append")
	}
}
