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

package controller

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
)

// TestProbeDebounce_FailureAndRecovery exercises ADR-0012: once an effective
// Healthy status is established, a failing streak flips lastProbeResult to Failed
// only after failureThreshold cycles, and a Healthy streak flips it back only after
// recoveryThreshold cycles. Uses the built-in defaults (3 / 2).
func TestProbeDebounce_FailureAndRecovery(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	r, c := newProbeReconciler(t, prober, probeCR(nil), probeRobotObj("amr-1"))

	// Cycle 1: Healthy establishes the effective baseline.
	reconcileProbe(t, r)
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusHealthy) {
		t.Fatalf("after healthy: result = %s, want Healthy", st.LastProbeResult)
	}

	// Now fail. Cycles 2 and 3 (streak 1,2 < 3) hold the debounced Healthy even
	// though the raw per-robot result is Failed.
	prober.result = probe.Result{}
	prober.err = errors.New("unreachable")
	for i := 1; i <= 2; i++ {
		reconcileProbe(t, r)
		st := probeResult(t, c)
		if string(st.LastProbeResult) != string(probe.StatusHealthy) {
			t.Fatalf("failing cycle %d: result = %s, want Healthy (debounced)", i, st.LastProbeResult)
		}
		if string(st.RobotResults[0].ProbeStatus) != string(probe.StatusFailed) {
			t.Fatalf("failing cycle %d: raw result = %s, want Failed (undebounced)", i, st.RobotResults[0].ProbeStatus)
		}
	}
	// Cycle 4: streak reaches 3 → flip to Failed.
	reconcileProbe(t, r)
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusFailed) {
		t.Fatalf("third failing cycle: result = %s, want Failed", st.LastProbeResult)
	}

	// Recover. First Healthy cycle (streak 1 < 2) holds Failed; second flips Healthy.
	prober.result = probe.Result{Status: probe.StatusHealthy}
	prober.err = nil
	reconcileProbe(t, r)
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusFailed) {
		t.Fatalf("first recovery cycle: result = %s, want Failed (debounced)", st.LastProbeResult)
	}
	reconcileProbe(t, r)
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusHealthy) {
		t.Fatalf("second recovery cycle: result = %s, want Healthy", st.LastProbeResult)
	}
}

// TestProbeDebounce_PerProbeThresholdWins verifies a per-probe FailureThreshold=1
// makes the flip immediate, overriding the namespace default.
func TestProbeDebounce_PerProbeThresholdWins(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	rp := probeCR(nil)
	rp.Spec.FailureThreshold = p32(1)
	cfg := configWithSpec(rolloutNS, fleetv1.SwarmadaConfigSpec{
		Health: fleetv1.SwarmadaHealthConfig{DefaultProbeFailureThreshold: 5},
	})
	r, c := newProbeReconciler(t, prober, rp, cfg, probeRobotObj("amr-1"))

	reconcileProbe(t, r) // establish Healthy
	prober.result = probe.Result{}
	prober.err = errors.New("down")
	reconcileProbe(t, r) // one failing cycle; threshold 1 → immediate Failed
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusFailed) {
		t.Fatalf("result = %s, want Failed (per-probe threshold 1)", st.LastProbeResult)
	}
}

// TestProbeThresholds_Resolution unit-tests the precedence: per-probe > namespace >
// constant, and the fail-safe when no config is present.
func TestProbeThresholds_Resolution(t *testing.T) {
	tests := []struct {
		name         string
		perProbeFail *int32
		perProbeRec  *int32
		config       *fleetv1.SwarmadaConfig
		wantFail     int32
		wantRec      int32
	}{
		{name: "no config, no per-probe → constants", wantFail: 3, wantRec: 2},
		{
			name:     "namespace default used",
			config:   configWithSpec(rolloutNS, fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DefaultProbeFailureThreshold: 7, DefaultProbeRecoveryThreshold: 4}}),
			wantFail: 7, wantRec: 4,
		},
		{
			name:         "per-probe overrides namespace",
			perProbeFail: p32(9), perProbeRec: p32(6),
			config:   configWithSpec(rolloutNS, fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DefaultProbeFailureThreshold: 7, DefaultProbeRecoveryThreshold: 4}}),
			wantFail: 9, wantRec: 6,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rp := probeCR(nil)
			rp.Spec.FailureThreshold = tc.perProbeFail
			rp.Spec.RecoveryThreshold = tc.perProbeRec
			objs := []client.Object{rp, probeRobotObj("amr-1")}
			if tc.config != nil {
				objs = append(objs, tc.config)
			}
			r, _ := newProbeReconciler(t, &fakeProber{}, objs...)
			gotFail, gotRec := r.probeThresholds(context.Background(), rp)
			if gotFail != tc.wantFail || gotRec != tc.wantRec {
				t.Errorf("probeThresholds = (%d, %d), want (%d, %d)", gotFail, gotRec, tc.wantFail, tc.wantRec)
			}
		})
	}
}
