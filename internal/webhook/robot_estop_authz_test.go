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

// robotEstopZone is the leaf FleetZone the estop fixtures sit in. It exists in estopGate's
// client so the Robot is admissible on its own merits: the invariant half of the gate
// (zone resolution and leafness, robot-id uniqueness) runs on EVERY update, including
// an estop-annotation-only one. These tests must fail on the estop verb or not at all.
const robotEstopZone = "aisle-b3"

// robotForEstop builds a Robot with (or without) the estop-triggered annotation.
// class/adapter are left empty so an estop-annotation-only update leaves the binding
// unchanged and never re-runs the liveness-dependent adapter gate.
func robotForEstop(name, triggerVal string) *fleetv1.Robot {
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.RobotSpec{Zone: robotEstopZone},
	}
	if triggerVal != "" {
		r.Annotations = map[string]string{estopTriggeredAnnotation: triggerVal}
	}
	return r
}

// estopGate builds the gate with a client that can resolve robotEstopZone (zoneNS and the
// crossobject fixtures' xoNS are the same namespace, so xoZone/xoClient apply directly).
func estopGate(t *testing.T, authz VerbAuthorizer) *RobotAdmissionGate {
	t.Helper()
	return &RobotAdmissionGate{Client: xoClient(t, xoZone(robotEstopZone, "")), EstopAuthz: authz}
}

// Setting the annotation requires the estop-trigger verb; an authorized user passes.
func TestRobotEstop_TriggerAllowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	g := estopGate(t, authz)

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
	g := estopGate(t, &fakeEstopAuthz{allow: false})
	if _, err := g.ValidateUpdate(ctxAsUser("mallory"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "stop")); err == nil {
		t.Fatal("unauthorized estop-trigger must be denied")
	}
}

// Removing the annotation requires the stricter estop-clear verb.
func TestRobotEstop_ClearRequiresClearVerb(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	g := estopGate(t, authz)

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
	g := estopGate(t, nil)
	if _, err := g.ValidateUpdate(ctxAsUser("alice"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "stop")); err == nil {
		t.Fatal("a nil estop authorizer must fail closed")
	}
}

// A non-estop update is not gated and never calls the authorizer.
func TestRobotEstop_NonEstopUpdateNotGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny if consulted
	g := estopGate(t, authz)

	if _, err := g.ValidateUpdate(ctxAsUser("alice"),
		robotForEstop("amr-1", ""), robotForEstop("amr-1", "")); err != nil {
		t.Fatalf("a non-estop update must not be gated, got: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authorizer called %d times on a non-estop update, want 0", authz.calls)
	}
}
