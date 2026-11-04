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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// GuardEstopProcessedMarker is proved in isolation by
// estop_marker_guard_test.go; these assert it is actually REACHED from each of the three
// carriers' validating webhooks. A guard nothing calls is not a guard.
//
// The forged write is the dangerous shape, and it is why the call cannot live inside the
// `isEstopChange` branch beside authorizeEstopVerb: an attacker pre-stamps the marker on a
// quiet update that touches the estop-TRIGGERED annotation not at all, so the verb gate is
// never consulted. The marker then suppresses the fan-out of the trigger that follows —
// no estop delivered, no audit entry. That is also the durable half of the estop write-ordering guarantee's
// ordering guarantee: the in-process epoch resets on a leader change, and a new leader
// re-reconciles from object state, where this marker is what stops a completed episode
// replaying.

func markedZone(name, marker string) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name}}
	if marker != "" {
		z.Annotations = map[string]string{estopProcessedAnnotation: marker}
	}
	return z
}

func markedRobot(name, marker string) *fleetv1.Robot {
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.RobotSpec{Zone: robotEstopZone},
	}
	if marker != "" {
		r.Annotations = map[string]string{estopProcessedAnnotation: marker}
	}
	return r
}

func markedConfig(marker string) *fleetv1.SwarmadaConfig {
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "swarmada"}}
	if marker != "" {
		cfg.Annotations = map[string]string{estopProcessedAnnotation: marker}
	}
	return cfg
}

// A client forging the marker is refused at each carrier's admission webhook.
func TestEstopMarkerWiring_ClientForgeryRefusedOnEveryCarrier(t *testing.T) {
	t.Run("fleetzone", func(t *testing.T) {
		// allow:true so a denial can only come from the marker guard, never the verb gate.
		v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: true})
		_, err := v.ValidateUpdate(ctxAsUser("mallory"), markedZone("dock", ""), markedZone("dock", "e1"))
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a client write of the estop marker on a FleetZone must be Forbidden, got: %v", err)
		}
	})
	t.Run("robot", func(t *testing.T) {
		g := estopGate(t, &fakeEstopAuthz{allow: true})
		_, err := g.ValidateUpdate(ctxAsUser("mallory"), markedRobot("amr-1", ""), markedRobot("amr-1", "e1"))
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a client write of the estop marker on a Robot must be Forbidden, got: %v", err)
		}
	})
	t.Run("swarmadaconfig", func(t *testing.T) {
		v := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
		_, err := v.ValidateUpdate(ctxAsUser("mallory"), markedConfig(""), markedConfig("e1"))
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a client write of the estop marker on a SwarmadaConfig must be Forbidden, got: %v", err)
		}
	})
}

// Removing the marker is forgery too: dropping it makes a completed episode replay on the
// next reconcile, which re-delivers an estop nobody asked for.
func TestEstopMarkerWiring_ClientRemovalRefusedOnEveryCarrier(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: true})
	if _, err := v.ValidateUpdate(ctxAsUser("alice"), markedZone("dock", "e1"), markedZone("dock", "")); err == nil {
		t.Error("a client removing the FleetZone estop marker must be refused")
	}
	g := estopGate(t, &fakeEstopAuthz{allow: true})
	if _, err := g.ValidateUpdate(ctxAsUser("alice"), markedRobot("amr-1", "e1"), markedRobot("amr-1", "")); err == nil {
		t.Error("a client removing the Robot estop marker must be refused")
	}
	c := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
	if _, err := c.ValidateUpdate(ctxAsUser("alice"), markedConfig("e1"), markedConfig("")); err == nil {
		t.Error("a client removing the SwarmadaConfig estop marker must be refused")
	}
}

// The controller's own write is how the fan-out records that a trigger value has been acted
// on; wiring the guard must not break it.
func TestEstopMarkerWiring_ControllerWriteStillAdmitted(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: true})
	if _, err := v.ValidateUpdate(ctxAsUser(controllerUser), markedZone("dock", ""), markedZone("dock", "e1")); err != nil {
		t.Errorf("the controller must still be able to stamp a FleetZone: %v", err)
	}
	g := estopGate(t, &fakeEstopAuthz{allow: true})
	if _, err := g.ValidateUpdate(ctxAsUser(controllerUser), markedRobot("amr-1", ""), markedRobot("amr-1", "e1")); err != nil {
		t.Errorf("the controller must still be able to stamp a Robot: %v", err)
	}
	c := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
	if _, err := c.ValidateUpdate(ctxAsUser(controllerUser), markedConfig(""), markedConfig("e1")); err != nil {
		t.Errorf("the controller must still be able to stamp a SwarmadaConfig: %v", err)
	}
}

// An update that leaves the marker alone is not a marker write, whoever sends it. Without
// this the guard would deny every unrelated edit of an object that is currently estopped.
func TestEstopMarkerWiring_UnrelatedUpdateStillAdmitted(t *testing.T) {
	v := newZoneValidatorWithAuthz(&fakeEstopAuthz{allow: true})
	if _, err := v.ValidateUpdate(ctxAsUser("alice"), markedZone("dock", "e1"), markedZone("dock", "e1")); err != nil {
		t.Errorf("an update leaving the FleetZone marker unchanged must be admitted: %v", err)
	}
	g := estopGate(t, &fakeEstopAuthz{allow: true})
	if _, err := g.ValidateUpdate(ctxAsUser("alice"), markedRobot("amr-1", "e1"), markedRobot("amr-1", "e1")); err != nil {
		t.Errorf("an update leaving the Robot marker unchanged must be admitted: %v", err)
	}
	c := &SwarmadaConfigValidator{EstopAuthz: &fakeEstopAuthz{allow: true}}
	if _, err := c.ValidateUpdate(ctxAsUser("alice"), markedConfig("e1"), markedConfig("e1")); err != nil {
		t.Errorf("an update leaving the SwarmadaConfig marker unchanged must be admitted: %v", err)
	}
}
