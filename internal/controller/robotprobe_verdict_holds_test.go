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

// A probe verdict holds until the probe clears it
// (RFC-0001 #robotprobe-probe-failure-effect-what-happens-downstream).
//
// A hardware RobotProbe that crosses its failure threshold degrades the component on
// Robot.status.hardware[]. The adapter may still report that component healthy, which is
// the case a probe exists to catch, so telemetry must not put it back: not on the next
// material change, and not on the first frame after a manager restart, when the
// projector's in-memory state is empty and its first write carries the adapter's full map.
// Only the probe's own recovery clears it.

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// verdictManager is the in-memory half of a manager: a telemetry pipeline and a probe
// controller. A restart is a new one over the same API server.
type verdictManager struct {
	ing   *telemetry.Ingestor
	probe *RobotProbeReconciler
	clock time.Time
}

func newVerdictManager(prober probe.Prober) *verdictManager {
	m := &verdictManager{clock: time.Now()}
	m.ing = telemetry.NewIngestor(nil, telemetry.NewProjector(telemetry.DefaultConfig()),
		&RobotStatusSink{Client: envK8s})
	m.probe = &RobotProbeReconciler{Client: envK8s, Scheme: envScheme, Prober: prober,
		now: func() time.Time { return m.clock }}
	return m
}

func (m *verdictManager) frame(t *testing.T, ns string, hw map[string]fleetv1.HardwareStatus) {
	t.Helper()
	if err := m.ing.Ingest(context.Background(), telemetry.Frame{
		RobotID: "rid-1", Namespace: ns, Hardware: hw,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// cycle runs one probe cycle, an hour after the last, so the pacing gate never holds it.
func (m *verdictManager) cycle(t *testing.T, ns string) {
	t.Helper()
	m.clock = m.clock.Add(time.Hour)
	if _, err := m.probe.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "probe"}}); err != nil {
		t.Fatalf("probe cycle: %v", err)
	}
}

func camFront(t *testing.T, ns string) fleetv1.HardwareComponentStatus {
	t.Helper()
	var r fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "amr-1"}, &r); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return hwByName(&r)["cam-front"]
}

func TestEnvtest_ProbeDegradedHoldsAcrossTelemetryAndRestart(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestProbeRobot(ns, "amr-1", "rid-1")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	rp := envtestProbe(ns, fleetv1.ProbeTypeHardware)
	rp.Spec.FailureThreshold, rp.Spec.RecoveryThreshold = i32(2), i32(2)
	if err := envK8s.Create(ctx, rp); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	// The adapter reports the camera healthy; a failing probe says otherwise.
	prober := &fakeProber{result: probe.Result{Status: probe.StatusFailed, Message: "frame_rate_pct=48 (threshold 80)"}}
	m := newVerdictManager(prober)
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	if h := camFront(t, ns); h.Status != fleetv1.HardwareHealthy {
		t.Fatalf("precondition: cam-front = %s, want Healthy from telemetry", h.Status)
	}
	m.cycle(t, ns)
	m.cycle(t, ns)
	h := camFront(t, ns)
	if h.Status != fleetv1.HardwareDegraded || h.DegradationReason == "" {
		t.Fatalf("cam-front = %s reason=%q after the probe crossed its threshold, want Degraded with a reason",
			h.Status, h.DegradationReason)
	}

	// 1. A telemetry change: a new component arrives, so the projector writes the full map,
	//    which still says the camera is healthy.
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"imu": fleetv1.HardwareHealthy})
	if h := camFront(t, ns); h.Status != fleetv1.HardwareDegraded {
		t.Fatalf("cam-front = %s after a telemetry change; the probe verdict must hold", h.Status)
	}

	// 2. A restart: new projector, new sink, new probe controller. The first frame writes
	//    the adapter's full map; the probe, still failing, has no new crossing to write.
	m = newVerdictManager(prober)
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{
		"cam-front": fleetv1.HardwareHealthy, "imu": fleetv1.HardwareHealthy})
	m.cycle(t, ns)
	if h := camFront(t, ns); h.Status != fleetv1.HardwareDegraded {
		t.Fatalf("cam-front = %s after a restart's first frame; the probe verdict must hold", h.Status)
	}

	// 3. Telemetry may make it worse.
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareFailed})
	if h := camFront(t, ns); h.Status != fleetv1.HardwareFailed {
		t.Fatalf("cam-front = %s after telemetry reported Failed, want Failed", h.Status)
	}
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	if h := camFront(t, ns); h.Status != fleetv1.HardwareDegraded {
		t.Fatalf("cam-front = %s when telemetry recovered under a held verdict, want Degraded", h.Status)
	}

	// 4. The probe clears it, and telemetry owns the component again.
	prober.result = probe.Result{Status: probe.StatusHealthy}
	m.cycle(t, ns)
	m.cycle(t, ns)
	if h := camFront(t, ns); h.Status != fleetv1.HardwareHealthy || h.DegradationReason != "" {
		t.Fatalf("cam-front = %s reason=%q after the probe recovered, want Healthy and no reason",
			h.Status, h.DegradationReason)
	}
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareDegraded})
	if h := camFront(t, ns); h.Status != fleetv1.HardwareDegraded {
		t.Fatalf("cam-front = %s; telemetry must again move a component no probe holds", h.Status)
	}
}

// A verdict whose probe is gone is released: nothing else could ever clear it.
func TestProbeVerdict_ReleasedWhenNoProbeReportsOnTheComponent(t *testing.T) {
	robot := robotWithID("amr-1", "warehouse-a", "rid-1")
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{
		{Name: "cam-front", Status: fleetv1.HardwareDegraded, DegradationReason: "frame_rate_pct=48"},
	}
	sink, c := newSink(t, robot)
	if _, err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "rid-1", Namespace: "warehouse-a",
		Hardware: map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if h := hwByName(getRobot(t, c, "amr-1", "warehouse-a"))["cam-front"]; h.Status != fleetv1.HardwareHealthy {
		t.Fatalf("cam-front = %s with no probe reporting on it, want telemetry's Healthy", h.Status)
	}
}

// The probe's reason is never empty, because the sink reads it as the verdict's mark.
func TestProbeVerdict_DegradeAlwaysCarriesAReason(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestProbeRobot(ns, "amr-1", "rid-1")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	rp := envtestProbe(ns, fleetv1.ProbeTypeHardware)
	rp.Spec.FailureThreshold = i32(1)
	if err := envK8s.Create(ctx, rp); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	m := newVerdictManager(&fakeProber{result: probe.Result{Status: probe.StatusFailed}})
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	m.cycle(t, ns)
	if h := camFront(t, ns); h.Status != fleetv1.HardwareDegraded || h.DegradationReason == "" {
		t.Fatalf("cam-front = %s reason=%q; a probe degrade with an empty adapter message must still carry a reason",
			h.Status, h.DegradationReason)
	}
}
