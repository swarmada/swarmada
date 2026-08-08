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
	"errors"
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/probe"
)

// PROBE_FAILURE (§9.6.5.1). A single failed probe is noise; the fact worth sealing is the
// SUSTAINED failure that crosses the threshold, because that is what demotes the component
// and takes its capabilities out of scheduling.

// degradedProber fails every probe, so repeated reconciles build a failure streak.
func degradedProber() *fakeProber {
	return &fakeProber{result: probe.Result{
		Status:        probe.StatusHealthy, // downgraded to Degraded by the metric check below
		ActualMetrics: map[string]float64{"frame_rate_pct": 40},
		Message:       "frame rate collapsed",
	}}
}

func TestAuditProbe_SealsOnTheThresholdCrossingOnly(t *testing.T) {
	rec := &recordingAudit{}
	rp := probeCR(map[string]string{"frame_rate_pct": "80"})
	robot := probeRobotObj("amr-1")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "cam-front", Type: "Camera"}}
	r, _ := newProbeReconciler(t, degradedProber(), rp, robot)
	r.Audit = rec

	// Below the default failure threshold (3): a flap must record nothing at all.
	reconcileProbe(t, r)
	reconcileProbe(t, r)
	if n := len(rec.ofType(audit.EventProbeFailure)); n != 0 {
		t.Fatalf("a sub-threshold failure streak must seal nothing, got %d", n)
	}

	reconcileProbe(t, r) // third failure → crossing
	got := rec.ofType(audit.EventProbeFailure)
	if len(got) != 1 {
		t.Fatalf("want one PROBE_FAILURE at the crossing, got %d", len(got))
	}
	e := got[0]
	if e.Resource.Kind != "Robot" || e.Resource.Name != "amr-1" {
		t.Fatalf("the entry concerns the robot, got %+v", e.Resource)
	}
	if e.Detail["probe_name"] != "cam-probe" {
		t.Errorf("probe_name = %q", e.Detail["probe_name"])
	}
	if e.Detail["consecutive_failures"] != "3" {
		t.Errorf("consecutive_failures = %q, want 3", e.Detail["consecutive_failures"])
	}
	// The streak clamps at the threshold, so every later tick looks identical to the
	// crossing. Without the edge, the chain would gain an entry per probe cycle forever.
	reconcileProbe(t, r)
	reconcileProbe(t, r)
	if n := len(rec.ofType(audit.EventProbeFailure)); n != 1 {
		t.Fatalf("a sustained failure must seal once, not once per cycle; got %d", n)
	}
}

func TestAuditProbe_FailedMetricsNamesWhichThresholdWasMissed(t *testing.T) {
	// "the probe failed" leaves an investigator re-running it to learn what a recorded
	// entry could have told them — by which time the robot's state has usually moved on.
	rec := &recordingAudit{}
	rp := probeCR(map[string]string{"frame_rate_pct": "80", "exposure_ok": "1"})
	robot := probeRobotObj("amr-1")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "cam-front", Type: "Camera"}}
	r, _ := newProbeReconciler(t, degradedProber(), rp, robot)
	r.Audit = rec

	for i := 0; i < 3; i++ {
		reconcileProbe(t, r)
	}
	got := rec.ofType(audit.EventProbeFailure)
	if len(got) != 1 {
		t.Fatalf("want one PROBE_FAILURE, got %d", len(got))
	}
	failed := got[0].Detail["failed_metrics"]
	// Below threshold: named with both readings, so the entry stands alone.
	if !strings.Contains(failed, "frame_rate_pct") {
		t.Errorf("the below-threshold metric must be named: %q", failed)
	}
	// Not reported at all: an absent reading and a low one are different faults, and the
	// entry must not let them look the same.
	if !strings.Contains(failed, "exposure_ok=missing") {
		t.Errorf("an unreported metric must be named as missing: %q", failed)
	}
}

func TestAuditProbe_HealthyProbeSealsNothing(t *testing.T) {
	rec := &recordingAudit{}
	prober := &fakeProber{result: probe.Result{
		Status: probe.StatusHealthy, ActualMetrics: map[string]float64{"frame_rate_pct": 95},
	}}
	rp := probeCR(map[string]string{"frame_rate_pct": "80"})
	robot := probeRobotObj("amr-1")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "cam-front", Type: "Camera"}}
	r, _ := newProbeReconciler(t, prober, rp, robot)
	r.Audit = rec

	for i := 0; i < 5; i++ {
		reconcileProbe(t, r)
	}
	if n := len(rec.ofType(audit.EventProbeFailure)); n != 0 {
		t.Fatalf("a passing probe must seal nothing, got %d", n)
	}
}

func TestAuditProbe_SinkFailureAndNilRecorderAreSafe(t *testing.T) {
	// The demotion this rides has already been decided; an audit sink must never be able
	// to stop a failing component being taken out of scheduling.
	rp := probeCR(map[string]string{"frame_rate_pct": "80"})
	robot := probeRobotObj("amr-1")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "cam-front", Type: "Camera"}}

	r, c := newProbeReconciler(t, degradedProber(), rp, robot)
	r.Audit = &recordingAudit{err: errors.New("sink unavailable")}
	for i := 0; i < 3; i++ {
		reconcileProbe(t, r)
	}
	if got := probeResult(t, c).ConsecutiveFailures; got != 3 {
		t.Fatalf("a failing sink changed the probe outcome: consecutiveFailures = %d", got)
	}

	r2, c2 := newProbeReconciler(t, degradedProber(), probeCR(map[string]string{"frame_rate_pct": "80"}),
		probeRobotObj("amr-1"))
	// r2.Audit deliberately nil.
	for i := 0; i < 3; i++ {
		reconcileProbe(t, r2)
	}
	if got := probeResult(t, c2).ConsecutiveFailures; got != 3 {
		t.Fatalf("nil Audit changed the probe outcome: consecutiveFailures = %d", got)
	}
}
