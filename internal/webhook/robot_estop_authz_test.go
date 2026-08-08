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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// robotForEstop builds a Robot with (or without) the estop-triggered annotation;
// class/adapter are left empty so an estop-annotation-only update never reaches the
// adapter-binding gate (unchanged binding → admitted after the estop check).
func robotForEstop(name, triggerVal string) *fleetv1.Robot {
	r := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name}}
	if triggerVal != "" {
		r.Annotations = map[string]string{estopTriggeredAnnotation: triggerVal}
	}
	return r
}

// Setting the annotation requires the estop-trigger verb; an authorized user passes.
func TestRobotEstop_TriggerAllowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	g := &RobotAdmissionGate{EstopAuthz: authz}

	if _, err := g.ValidateUpdate(ctxAsUser("alice"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "person detected")); err != nil {
		t.Fatalf("authorized estop-trigger should be admitted, got: %v", err)
	}
	if authz.gotVerb != estopTriggerVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, estopTriggerVerb)
	}
}

// An unauthorized trigger is denied.
func TestRobotEstop_TriggerDenied(t *testing.T) {
	g := &RobotAdmissionGate{EstopAuthz: &fakeEstopAuthz{allow: false}}
	if _, err := g.ValidateUpdate(ctxAsUser("mallory"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "stop")); err == nil {
		t.Fatal("unauthorized estop-trigger must be denied")
	}
}

// Removing the annotation requires the stricter estop-clear verb.
func TestRobotEstop_ClearRequiresClearVerb(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	g := &RobotAdmissionGate{EstopAuthz: authz}

	if _, err := g.ValidateUpdate(ctxAsUser("admin"),
		robotForEstop("amr-1", "stop"), robotForEstop("amr-1", "")); err != nil {
		t.Fatalf("authorized estop-clear should be admitted, got: %v", err)
	}
	if authz.gotVerb != estopClearVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, estopClearVerb)
	}
}

// A nil authorizer fails closed — an estop annotation write is refused.
func TestRobotEstop_NilAuthzFailsClosed(t *testing.T) {
	g := &RobotAdmissionGate{}
	if _, err := g.ValidateUpdate(ctxAsUser("alice"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "stop")); err == nil {
		t.Fatal("a nil estop authorizer must fail closed")
	}
}

// A non-estop update is not gated and never calls the authorizer.
func TestRobotEstop_NonEstopUpdateNotGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny if consulted
	g := &RobotAdmissionGate{EstopAuthz: authz}

	if _, err := g.ValidateUpdate(ctxAsUser("alice"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "")); err != nil {
		t.Fatalf("a non-estop update must not be gated, got: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authorizer called %d times on a non-estop update, want 0", authz.calls)
	}
}
