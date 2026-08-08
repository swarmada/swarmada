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
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The connectivity thresholds define a two-stage response: Offline at T1, then Critical
// at T2 (RFC-0001 #safety-thresholds). With T2 <= T1 the escalation is due at or before
// the transition it escalates from, so the second stage can never be reached as a
// distinct event. Each field is bounded by CEL independently and the bounds OVERLAP —
// offline reaches 3600, critical starts at 30 — so CEL cannot catch an inverted pair.
func cfgWithThresholds(offline, critical int32) *fleetv1.SwarmadaConfig {
	c := &fleetv1.SwarmadaConfig{}
	c.Spec.Health.ConnectivityOfflineThresholdSeconds = offline
	c.Spec.Health.ConnectivityCriticalThresholdSeconds = critical
	return c
}

func TestConnectivityThresholds_OrderedPairIsAccepted(t *testing.T) {
	if _, err := validateSwarmadaConfig(cfgWithThresholds(30, 120)); err != nil {
		t.Fatalf("the documented defaults (30/120) must validate, got: %v", err)
	}
}

func TestConnectivityThresholds_InvertedPairIsRejected(t *testing.T) {
	// Both values are individually legal under CEL (offline<=3600, critical>=30).
	_, err := validateSwarmadaConfig(cfgWithThresholds(600, 60))
	if err == nil {
		t.Fatal("T2 < T1 was accepted; the graduated connectivity response is incoherent " +
			"and RFC-0001 #safety-thresholds states admission rejects it")
	}
	if !strings.Contains(err.Error(), "connectivityCriticalThresholdSeconds") {
		t.Fatalf("the error must name the offending field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "600") {
		t.Fatalf("the error must quote the value it must exceed so an operator can fix it, got: %v", err)
	}
}

func TestConnectivityThresholds_EqualIsRejected(t *testing.T) {
	// "Strictly greater" — equal thresholds fire both transitions on the same tick,
	// which is the collapse this rule exists to prevent, not a boundary that squeaks by.
	if _, err := validateSwarmadaConfig(cfgWithThresholds(120, 120)); err == nil {
		t.Fatal("T2 == T1 was accepted; the rule is strictly-greater")
	}
}

func TestConnectivityThresholds_AdjacentPairIsAccepted(t *testing.T) {
	// One second apart is ordered, so it must pass: the webhook enforces ordering, not
	// a minimum separation, and inventing one would reject configs the CRD permits.
	if _, err := validateSwarmadaConfig(cfgWithThresholds(30, 31)); err != nil {
		t.Fatalf("an ordered pair one second apart must validate, got: %v", err)
	}
}

func TestConnectivityThresholds_UnsetPairIsAccepted(t *testing.T) {
	// THE REGRESSION CASE. A zero-value config is what every programmatic caller builds
	// before the API server applies CEL defaults (30/120). Reading 0 <= 0 as an inverted
	// pair rejected it — a validator failing closed on the wrong condition. The CEL
	// minimums (5 and 30) make an explicit zero unrepresentable, so zero can only ever
	// mean unset by the time this runs.
	if _, err := validateSwarmadaConfig(&fleetv1.SwarmadaConfig{}); err != nil {
		t.Fatalf("a zero-value config must validate (defaults are applied before admission), got: %v", err)
	}
}

func TestConnectivityThresholds_OneSidedIsAccepted(t *testing.T) {
	// Only one side set: the other is still to be defaulted, so there is no pair to
	// compare yet. Comparing against a zero would reject a config the CRD permits.
	if _, err := validateSwarmadaConfig(cfgWithThresholds(0, 60)); err != nil {
		t.Fatalf("critical-only must validate, got: %v", err)
	}
	if _, err := validateSwarmadaConfig(cfgWithThresholds(60, 0)); err != nil {
		t.Fatalf("offline-only must validate, got: %v", err)
	}
}
