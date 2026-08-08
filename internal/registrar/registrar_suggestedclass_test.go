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

package registrar

// Discover derives DiscoveredRobot.status.suggestedRobotClass from the VERIFIED
// adapter's own FleetAdapter.spec.servesRobotClasses (ADR-0027) — a single entry
// suggests that class; zero/multiple/unknown leave it empty (no auto-admit).
// (newRegistrar, adapterID, getDR, regNS, regRobotID live in registrar_test.go.)

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func fleetAdapter(name string, classes ...string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: regNS},
		Spec:       fleetv1.FleetAdapterSpec{ServesRobotClasses: classes},
	}
}

func verifiedTLS(adapter string) controlstream.TLSIdentity {
	return controlstream.TLSIdentity{AdapterName: adapter, Namespace: regNS, Verified: true}
}

func discover(t *testing.T, r *Registrar, tlsID controlstream.TLSIdentity) *fav1.DiscoverAck {
	t.Helper()
	return r.Discover(context.Background(), adapterID(), tlsID, &fav1.DiscoverRobot{RobotId: regRobotID})
}

// A FleetAdapter serving exactly one class suggests it — so ADR-0014 auto-admit
// can fire.
func TestDiscover_SuggestedClass_SingleServesClass(t *testing.T) {
	r, c := newRegistrar(t, fleetAdapter("amr-adapter", "amr-device"))
	if ack := discover(t, r, verifiedTLS("amr-adapter")); !ack.GetAccepted() {
		t.Fatalf("Discover not accepted: %+v", ack)
	}
	if got := getDR(t, c, regRobotID).Status.SuggestedRobotClass; got != "amr-device" {
		t.Errorf("SuggestedRobotClass = %q, want %q", got, "amr-device")
	}
}

// Zero or multiple served classes are ambiguous → empty (operator admission).
func TestDiscover_SuggestedClass_ZeroOrMultipleLeavesEmpty(t *testing.T) {
	cases := []struct {
		name    string
		classes []string
	}{
		{"no classes", nil},
		{"two classes", []string{"amr-device", "cleaning-robot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newRegistrar(t, fleetAdapter("multi-adapter", tc.classes...))
			if ack := discover(t, r, verifiedTLS("multi-adapter")); !ack.GetAccepted() {
				t.Fatalf("Discover not accepted: %+v", ack)
			}
			if got := getDR(t, c, regRobotID).Status.SuggestedRobotClass; got != "" {
				t.Errorf("SuggestedRobotClass = %q, want empty (ambiguous serves-list)", got)
			}
		})
	}
}

// No FleetAdapter for the identity → empty class, and discovery still succeeds
// (fail safe: a missing/lookup-failed adapter never blocks the announce).
func TestDiscover_SuggestedClass_NoFleetAdapterSucceedsEmpty(t *testing.T) {
	r, c := newRegistrar(t) // no FleetAdapter object
	ack := discover(t, r, verifiedTLS("ghost-adapter"))
	if !ack.GetAccepted() {
		t.Fatalf("discovery must succeed even without a FleetAdapter: %+v", ack)
	}
	if got := getDR(t, c, regRobotID).Status.SuggestedRobotClass; got != "" {
		t.Errorf("SuggestedRobotClass = %q, want empty (no adapter)", got)
	}
}

// An unverified identity (empty AdapterName) never suggests a class, even if a
// single-class adapter exists — the class is derived only from the verified name.
func TestDiscover_SuggestedClass_UnverifiedIdentityLeavesEmpty(t *testing.T) {
	r, c := newRegistrar(t, fleetAdapter("amr-adapter", "amr-device"))
	if ack := discover(t, r, controlstream.TLSIdentity{}); !ack.GetAccepted() {
		t.Fatalf("Discover not accepted: %+v", ack)
	}
	if got := getDR(t, c, regRobotID).Status.SuggestedRobotClass; got != "" {
		t.Errorf("SuggestedRobotClass = %q, want empty for an unverified identity", got)
	}
}
