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

// status.hardware has two writers, the telemetry status sink and the RobotProbe
// controller, and both read the robot from the cache. A merge patch replaces the whole
// list, so a write made from a view older than the other writer's last write put back
// what that view held: a Failed component the robot had just reported came back Healthy,
// and the resume check, which refuses while a component is Failed, could pass
// (RFC-0001 #safety-estop-state-machine). The projector does not report an unchanged
// Failed again, so nothing restored it. Each writer now writes under the resourceVersion
// it read, and on a conflict reads the robot again, uncached, and redoes the change.

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// staleRobotClient serves one robot from a fixed snapshot, to Get and to List, as a
// lagging informer would, and passes everything else, all writes included, to the API
// server. It counts the status writes the API server stored.
type staleRobotClient struct {
	client.Client
	stale  *fleetv1.Robot
	stored int
}

func (c *staleRobotClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r, ok := obj.(*fleetv1.Robot); ok && key == client.ObjectKeyFromObject(c.stale) {
		c.stale.DeepCopyInto(r)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *staleRobotClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if rl, ok := list.(*fleetv1.RobotList); ok {
		for i := range rl.Items {
			if client.ObjectKeyFromObject(&rl.Items[i]) == client.ObjectKeyFromObject(c.stale) {
				c.stale.DeepCopyInto(&rl.Items[i])
			}
		}
	}
	return nil
}

func (c *staleRobotClient) Status() client.SubResourceWriter {
	return &storedStatusWrites{SubResourceWriter: c.Client.Status(), c: c}
}

type storedStatusWrites struct {
	client.SubResourceWriter
	c *staleRobotClient
}

func (w *storedStatusWrites) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	if err == nil {
		w.c.stored++
	}
	return err
}

func setHardware(t *testing.T, ns, name string, hw ...fleetv1.HardwareComponentStatus) {
	t.Helper()
	r := getEnvRobot(t, ns, name)
	r.Status.Hardware = hw
	if err := envK8s.Status().Update(context.Background(), r); err != nil {
		t.Fatalf("writing hardware: %v", err)
	}
}

func TestEnvtest_ProbeHardwareWriteOverAStaleViewKeepsTheFailedEntry(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	setHardware(t, ns, "amr-1",
		fleetv1.HardwareComponentStatus{Name: "lidar", Status: fleetv1.HardwareHealthy},
		fleetv1.HardwareComponentStatus{Name: "camera", Status: fleetv1.HardwareHealthy})
	stale := getEnvRobot(t, ns, "amr-1") // the view a lagging informer keeps
	// The robot reports its lidar Failed, and the status sink writes it.
	setHardware(t, ns, "amr-1",
		fleetv1.HardwareComponentStatus{Name: "lidar", Status: fleetv1.HardwareFailed},
		fleetv1.HardwareComponentStatus{Name: "camera", Status: fleetv1.HardwareHealthy})

	c := &staleRobotClient{Client: envK8s, stale: stale}
	pr := &RobotProbeReconciler{Client: c, APIReader: envK8s, Scheme: envScheme}
	const reason = "probe camera-check failed 3 consecutive cycles"
	degradeCamera := func(view *fleetv1.Robot) {
		t.Helper()
		if err := pr.propagateHardwareStatus(ctx, view.DeepCopy(), "camera", fleetv1.HardwareDegraded, reason, nil); err != nil {
			t.Fatalf("the probe's hardware write: %v", err)
		}
	}
	degradeCamera(stale)
	hw := hwByName(getEnvRobot(t, ns, "amr-1"))
	if hw["lidar"].Status != fleetv1.HardwareFailed {
		t.Fatalf("lidar = %s after the probe wrote camera from a view older than the lidar's Failed report, want Failed",
			hw["lidar"].Status)
	}
	if hw["camera"].Status != fleetv1.HardwareDegraded || hw["camera"].DegradationReason != reason {
		t.Fatalf("camera = %s reason=%q, want the probe's Degraded verdict", hw["camera"].Status, hw["camera"].DegradationReason)
	}
	if c.stored != 1 {
		t.Fatalf("%d status writes stored for one crossing, want 1", c.stored)
	}

	// RA-1: a pass that changes nothing writes nothing, from the stale view or a current one.
	c.stored = 0
	degradeCamera(stale)
	degradeCamera(getEnvRobot(t, ns, "amr-1"))
	if c.stored != 0 {
		t.Fatalf("%d status writes stored by passes that changed nothing, want 0", c.stored)
	}

	// A probe crossing does not lower a component the robot reported Failed: only the
	// robot's own report does.
	for _, st := range []fleetv1.HardwareStatus{fleetv1.HardwareDegraded, fleetv1.HardwareHealthy} {
		if err := pr.propagateHardwareStatus(ctx, getEnvRobot(t, ns, "amr-1"), "lidar", st, "probe lidar-check", nil); err != nil {
			t.Fatalf("the probe's lidar write: %v", err)
		}
		if got := hwByName(getEnvRobot(t, ns, "amr-1"))["lidar"].Status; got != fleetv1.HardwareFailed {
			t.Fatalf("lidar = %s after a probe crossing to %s, want the robot's Failed", got, st)
		}
	}
}

// The sink's own reports carry the robot's full component map, so what a stale view
// erases there is the other writer's: the probe's verdict on a component the robot calls
// healthy, which only the probe clears.
func TestEnvtest_SinkHardwareWriteOverAStaleViewKeepsTheProbeVerdict(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestProbeRobot(ns, "amr-1", "rid-1")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	rp := envtestProbe(ns, fleetv1.ProbeTypeHardware)
	if err := envK8s.Create(ctx, rp); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	rp.Status.RobotResults = []fleetv1.RobotProbeRobotResult{{RobotName: "amr-1", Namespace: ns, ProbeStatus: fleetv1.ProbeResultDegraded}}
	if err := envK8s.Status().Update(ctx, rp); err != nil {
		t.Fatalf("probe results: %v", err)
	}
	setHardware(t, ns, "amr-1",
		fleetv1.HardwareComponentStatus{Name: "cam-front", Status: fleetv1.HardwareHealthy},
		fleetv1.HardwareComponentStatus{Name: "imu", Status: fleetv1.HardwareHealthy})
	stale := getEnvRobot(t, ns, "amr-1")
	// The probe crosses its threshold and degrades the camera.
	const reason = "probe probe failed 2 consecutive cycles"
	setHardware(t, ns, "amr-1",
		fleetv1.HardwareComponentStatus{Name: "cam-front", Status: fleetv1.HardwareDegraded, DegradationReason: reason},
		fleetv1.HardwareComponentStatus{Name: "imu", Status: fleetv1.HardwareHealthy})

	c := &staleRobotClient{Client: envK8s, stale: stale}
	sink := &RobotStatusSink{Client: c, APIReader: envK8s}
	report := telemetry.MaterialUpdate{Namespace: ns, RobotID: "rid-1", Hardware: map[string]fleetv1.HardwareStatus{
		"cam-front": fleetv1.HardwareHealthy, "imu": fleetv1.HardwareDegraded}}
	if _, err := sink.ApplyMaterialUpdate(ctx, report); err != nil {
		t.Fatalf("the sink's write: %v", err)
	}
	hw := hwByName(getEnvRobot(t, ns, "amr-1"))
	if hw["cam-front"].Status != fleetv1.HardwareDegraded || hw["cam-front"].DegradationReason != reason {
		t.Fatalf("cam-front = %s reason=%q after the sink wrote from a view older than the probe's verdict, want the verdict",
			hw["cam-front"].Status, hw["cam-front"].DegradationReason)
	}
	if hw["imu"].Status != fleetv1.HardwareDegraded {
		t.Fatalf("imu = %s, want the reported Degraded", hw["imu"].Status)
	}
	if c.stored != 1 {
		t.Fatalf("%d status writes stored for one report, want 1", c.stored)
	}

	// RA-1: the same report again changes nothing and writes nothing.
	c.stored = 0
	wrote, err := sink.ApplyMaterialUpdate(ctx, report)
	if err != nil {
		t.Fatalf("the sink's second write: %v", err)
	}
	if wrote || c.stored != 0 {
		t.Fatalf("wrote=%v with %d status writes stored for a report that changed nothing, want none", wrote, c.stored)
	}
}

// A component the robot reported Failed is not stuck: the robot's own report of it healthy
// clears it, with or without a probe verdict on it. When the probe recovers while the
// component is Failed, it keeps Failed and releases its verdict, so the robot's next
// healthy report is not held at Degraded.
func TestEnvtest_AFailedComponentRecoversWhenTheRobotReportsItHealthy(t *testing.T) {
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
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	m := newVerdictManager(prober)
	status := func() fleetv1.HardwareComponentStatus { t.Helper(); return camFront(t, ns) }

	// 1. No probe verdict: Failed, then healthy again, from the robot's own reports.
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	m.cycle(t, ns)
	m.cycle(t, ns)
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareFailed})
	if h := status(); h.Status != fleetv1.HardwareFailed {
		t.Fatalf("cam-front = %s after the robot reported Failed", h.Status)
	}
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	if h := status(); h.Status != fleetv1.HardwareHealthy {
		t.Fatalf("cam-front = %s after the robot reported it Healthy again; a Failed entry must clear", h.Status)
	}

	// 2. A probe verdict, the robot then reports Failed, the probe recovers while it is
	//    Failed, and the robot reports it healthy.
	prober.result = probe.Result{Status: probe.StatusFailed, Message: "frame_rate_pct=48"}
	m.cycle(t, ns)
	m.cycle(t, ns)
	if h := status(); h.Status != fleetv1.HardwareDegraded || h.DegradationReason == "" {
		t.Fatalf("cam-front = %s reason=%q, want the probe's Degraded verdict", h.Status, h.DegradationReason)
	}
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareFailed})
	prober.result = probe.Result{Status: probe.StatusHealthy}
	m.cycle(t, ns)
	m.cycle(t, ns)
	if h := status(); h.Status != fleetv1.HardwareFailed || h.DegradationReason != "" {
		t.Fatalf("cam-front = %s reason=%q after the probe recovered, want the robot's Failed and the verdict released",
			h.Status, h.DegradationReason)
	}
	m.frame(t, ns, map[string]fleetv1.HardwareStatus{"cam-front": fleetv1.HardwareHealthy})
	if h := status(); h.Status != fleetv1.HardwareHealthy {
		t.Fatalf("cam-front = %s after the robot reported it Healthy; the Failed entry is stuck", h.Status)
	}
}
