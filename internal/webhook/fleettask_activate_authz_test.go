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
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func draftOf(draft bool) *fleetv1.FleetTask {
	t := ftWith("t1", ftAct("a"))
	t.Spec.Draft = draft
	return t
}

func ctxAs(username string, groups ...string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: username, Groups: groups},
		},
	})
}

// A person holding activate clears spec.draft; the verb asked about is activate.
func TestFleetTaskActivate_PersonWithVerbAdmitted(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := &FleetTaskValidator{ActivateAuthz: authz}
	if _, err := v.ValidateUpdate(ctxAs("alice"), draftOf(true), draftOf(false)); err != nil {
		t.Fatalf("a person holding activate was refused: %v", err)
	}
	if authz.gotVerb != activateVerb || authz.gotUser != "alice" {
		t.Errorf("asked %q for %q, want %q for alice", authz.gotVerb, authz.gotUser, activateVerb)
	}
}

// A person without the verb is refused.
func TestFleetTaskActivate_PersonWithoutVerbRefused(t *testing.T) {
	v := &FleetTaskValidator{ActivateAuthz: &fakeEstopAuthz{allow: false}}
	_, err := v.ValidateUpdate(ctxAs("mallory"), draftOf(true), draftOf(false))
	if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "activate") {
		t.Fatalf("activation without the verb must be Forbidden naming activate, got: %v", err)
	}
}

// A ServiceAccount is refused even when RBAC would allow it, by username form and by group,
// and RBAC is not consulted.
func TestFleetTaskActivate_ServiceAccountRefusedWhateverItHolds(t *testing.T) {
	for _, ctx := range []context.Context{
		ctxAs("system:serviceaccount:ops:drafter", "system:serviceaccounts", "system:serviceaccounts:ops"),
		ctxAs("system:serviceaccount:malformed"),
		ctxAs("odd-name", "system:serviceaccounts"),
	} {
		authz := &fakeEstopAuthz{allow: true}
		v := &FleetTaskValidator{ActivateAuthz: authz}
		_, err := v.ValidateUpdate(ctx, draftOf(true), draftOf(false))
		if err == nil || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ServiceAccount") {
			req, _ := admission.RequestFromContext(ctx)
			t.Errorf("%q %v activated a draft, or was refused for another reason: %v",
				req.UserInfo.Username, req.UserInfo.Groups, err)
		}
		if authz.calls != 0 {
			t.Errorf("RBAC was consulted for a ServiceAccount; the refusal must not depend on it")
		}
	}
}

// Fail closed: no authorizer, or an authorizer error, refuses.
func TestFleetTaskActivate_FailsClosed(t *testing.T) {
	v := &FleetTaskValidator{}
	if _, err := v.ValidateUpdate(ctxAs("alice"), draftOf(true), draftOf(false)); err == nil || !apierrors.IsForbidden(err) {
		t.Errorf("a nil authorizer must refuse an activation, got: %v", err)
	}
	v = &FleetTaskValidator{ActivateAuthz: &fakeEstopAuthz{allow: true, err: errors.New("authorizer unreachable")}}
	if _, err := v.ValidateUpdate(ctxAs("alice"), draftOf(true), draftOf(false)); err == nil {
		t.Error("an authorizer error must refuse an activation")
	}
}

// Only the true -> false transition is gated: a write that leaves a draft a draft, a write
// to a task that was never a draft, and re-drafting are not activations.
func TestFleetTaskActivate_OnlyTheTransitionIsGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false}
	v := &FleetTaskValidator{ActivateAuthz: authz}
	labelled := draftOf(true)
	labelled.Labels = map[string]string{"seen": "yes"}
	for name, pair := range map[string][2]*fleetv1.FleetTask{
		"a label on a draft":      {draftOf(true), labelled},
		"a write to a live task":  {draftOf(false), draftOf(false)},
		"setting draft on a task": {draftOf(false), draftOf(true)},
	} {
		if _, err := v.ValidateUpdate(ctxAs("system:serviceaccount:ops:bot", "system:serviceaccounts"), pair[0], pair[1]); err != nil {
			t.Errorf("%s was refused; it is not an activation: %v", name, err)
		}
	}
	if authz.calls != 0 {
		t.Errorf("authz consulted %d times on non-activations, want 0", authz.calls)
	}
}
