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

// nsConfigForEstop builds a minimal (cross-field-valid) SwarmadaConfig with or
// without the estop-triggered annotation.
func nsConfigForEstop(triggerVal string) *fleetv1.SwarmadaConfig {
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "swarmada"}}
	if triggerVal != "" {
		cfg.Annotations = map[string]string{estopTriggeredAnnotation: triggerVal}
	}
	return cfg
}

// Setting the annotation requires the estop-trigger verb; an authorized user passes.
func TestNamespaceEstop_TriggerAllowed(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := &SwarmadaConfigValidator{EstopAuthz: authz}

	if _, err := v.ValidateUpdate(ctxAsUser("alice"),
		nsConfigForEstop(""), nsConfigForEstop("evacuate")); err != nil {
		t.Fatalf("authorized estop-trigger should be admitted, got: %v", err)
	}
	if authz.gotVerb != estopTriggerVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, estopTriggerVerb)
	}
}

// An unauthorized namespace estop trigger is denied.
func TestNamespaceEstop_TriggerDenied(t *testing.T) {
	v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: false}}
	if _, err := v.ValidateUpdate(ctxAsUser("mallory"),
		nsConfigForEstop(""), nsConfigForEstop("stop")); err == nil {
		t.Fatal("unauthorized namespace estop-trigger must be denied")
	}
}

// Removing the annotation requires the stricter estop-clear verb.
func TestNamespaceEstop_ClearRequiresClearVerb(t *testing.T) {
	authz := &fakeEstopAuthz{allow: true}
	v := &SwarmadaConfigValidator{EstopAuthz: authz}

	if _, err := v.ValidateUpdate(ctxAsUser("admin"),
		nsConfigForEstop("stop"), nsConfigForEstop("")); err != nil {
		t.Fatalf("authorized estop-clear should be admitted, got: %v", err)
	}
	if authz.gotVerb != estopClearVerb {
		t.Errorf("verb = %q, want %q", authz.gotVerb, estopClearVerb)
	}
}

// A nil authorizer fails closed.
func TestNamespaceEstop_NilAuthzFailsClosed(t *testing.T) {
	v := &SwarmadaConfigValidator{}
	if _, err := v.ValidateUpdate(ctxAsUser("alice"),
		nsConfigForEstop(""), nsConfigForEstop("stop")); err == nil {
		t.Fatal("a nil estop authorizer must fail closed")
	}
}

// A non-estop update still validates but never consults the estop authorizer.
func TestNamespaceEstop_NonEstopUpdateNotGated(t *testing.T) {
	authz := &fakeEstopAuthz{allow: false} // would deny if consulted
	v := &SwarmadaConfigValidator{EstopAuthz: authz}

	if _, err := v.ValidateUpdate(ctxAsUser("alice"),
		nsConfigForEstop(""), nsConfigForEstop("")); err != nil {
		t.Fatalf("a non-estop update must not be gated, got: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("authorizer called %d times on a non-estop update, want 0", authz.calls)
	}
}
