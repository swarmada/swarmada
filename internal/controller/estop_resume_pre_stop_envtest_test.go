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
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The resume confirmation judges a robot against the stop it is leaving (RFC-0001
// #safety-estop-state-machine): every capability that was Active when the stop was
// taken must be Active again. A capability that was not Active before the stop does not
// hold the release, and one a maintenance window pauses does not either, because the
// window's close waits for the release. Each test takes the stop through the
// dispatcher, which is what records the capabilities Active at the stop; with no
// SafetyStream registered the stop resolves Failed, which is a stop.

// preStopFixture is a robot whose navigation needs its lidar and whose camera needs a
// camera_front its adapter never reports — the first-run robot's shape: the camera is
// Inactive from the start, and the robot runs work on navigation.
type preStopFixture struct {
	ns     string
	frames *telemetry.Router
	disp   *safety.Dispatcher
}

func newPreStopFixture(t *testing.T, caps ...fleetv1.ClassCapability) *preStopFixture {
	t.Helper()
	ctx := context.Background()
	f := &preStopFixture{ns: envtestNamespace(t), frames: resumeWitness()}
	f.disp = safety.New(envK8s, nil)
	f.disp.Telemetry = f.frames
	f.disp.DeriveCapabilities = DeriveCapabilities

	robot := envtestValidRobot(f.ns, "amr-1", "z-a")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{
		{Name: "lidar", Type: fleetv1.HardwareTypeLidar},
		{Name: "camera_front", Type: fleetv1.HardwareTypeCamera},
	}
	if len(caps) == 0 {
		caps = []fleetv1.ClassCapability{
			{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}},
			{Name: "camera", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"camera_front"}},
		}
	}
	robot.Spec.Capabilities = caps
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	seedResumeStatus(t, f.ns, "amr-1", fleetv1.RobotEstopNormal, time.Now())
	f.report(t, fleetv1.HardwareHealthy) // establishes status.hardware: lidar Healthy
	f.rederive(t)
	return f
}

// report sends one frame of the robot's own telemetry carrying its lidar reading.
func (f *preStopFixture) report(t *testing.T, lidar fleetv1.HardwareStatus) {
	t.Helper()
	if err := f.frames.Ingest(context.Background(), telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle,
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": lidar},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// rederive writes status.capabilities as the Robot reconciler derives them.
func (f *preStopFixture) rederive(t *testing.T) {
	t.Helper()
	robot := getEnvRobot(t, f.ns, "amr-1")
	robot.Status.Capabilities = DeriveCapabilities(robot)
	if err := envK8s.Status().Update(context.Background(), robot); err != nil {
		t.Fatalf("write derived capabilities: %v", err)
	}
}

// stopAndClear takes a stop through the dispatcher and clears it into Resuming.
func (f *preStopFixture) stopAndClear(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if res, err := f.disp.TriggerEstop(ctx, f.ns, "amr-1", "hazard", "alice", metrics.ScopeRobot, safety.Carrier{}); !errors.Is(err, safety.ErrUndelivered) ||
		res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("TriggerEstop = %+v (err %v), want Failed: no SafetyStream is registered", res, err)
	}
	if st, err := f.disp.ClearEstop(ctx, f.ns, "amr-1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", st, err)
	}
	time.Sleep(2 * time.Millisecond) // the witnessing frame must be strictly later than the clear
}

func (f *preStopFixture) confirm(t *testing.T) (fleetv1.RobotEstopState, string) {
	t.Helper()
	st, why, err := f.disp.ConfirmResume(context.Background(), f.ns, "amr-1", "alice")
	if err != nil {
		t.Fatalf("ConfirmResume: %v", err)
	}
	return st, why
}

// The first-run robot: its camera was Inactive before the stop, because its adapter
// never reports camera_front, and it ran work on navigation. The stop records
// navigation alone, and an operator's release after a healthy report takes the robot to
// Normal and removes the record.
func TestEnvtest_EstopResume_ACapabilityInactiveBeforeTheStopDoesNotHoldTheRelease(t *testing.T) {
	requireEnvtest(t)
	f := newPreStopFixture(t)
	f.stopAndClear(t)

	b := getEnvRobot(t, f.ns, "amr-1").Status.EstopBaseline
	if b == nil || !slices.Equal(b.ActiveCapabilities, []string{"navigation"}) {
		t.Fatalf("estopBaseline = %+v, want the stop to record navigation, the one capability Active at it", b)
	}

	f.report(t, fleetv1.HardwareHealthy)
	if st, why := f.confirm(t); st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal: camera was Inactive before the stop and cannot hold the release", st, why)
	}
	if b := getEnvRobot(t, f.ns, "amr-1").Status.EstopBaseline; b != nil {
		t.Fatalf("estopBaseline = %+v after Normal, want it removed", b)
	}
}

// A capability that was Active at the stop and is not Active now still refuses the
// release: with the lidar reported Failed (the hardware check refuses) and with the
// lidar reported Degraded, which fails nothing but the capability rule.
func TestEnvtest_EstopResume_ACapabilityActiveAtTheStopStillGatesTheRelease(t *testing.T) {
	requireEnvtest(t)
	for _, tc := range []struct {
		lidar   fleetv1.HardwareStatus
		refusal string
	}{
		{fleetv1.HardwareFailed, `hardware component "lidar" is reported Failed`},
		{fleetv1.HardwareDegraded, `capability "navigation" was Active when the stop was taken and is Degraded`},
	} {
		t.Run(string(tc.lidar), func(t *testing.T) {
			f := newPreStopFixture(t)
			f.stopAndClear(t)
			f.report(t, tc.lidar)
			st, why := f.confirm(t)
			if st != fleetv1.RobotEstopResuming || !strings.Contains(why, tc.refusal) {
				t.Fatalf("ConfirmResume = %s (%q), want Resuming refused with %q", st, why, tc.refusal)
			}
		})
	}
}

// The robot's post-clear report of a Failed lidar is not written: the status write
// fails. An operator who confirms before the robot's next report is refused, because
// the checks would read the status from before that report. The next frame writes the
// Failed reading, although it does not repeat it, and the release is still refused, now
// for the hardware; a report of the lidar repaired releases the robot.
func TestEnvtest_EstopResume_RefusedWhileTheLatestReportIsUnwritten(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	f := newPreStopFixture(t,
		fleetv1.ClassCapability{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}})
	sink := &failingStatusSink{inner: &RobotStatusSink{Client: envK8s}}
	f.frames = telemetry.NewRouter(func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} }, sink)
	f.disp.Telemetry = f.frames
	f.report(t, fleetv1.HardwareHealthy)
	f.stopAndClear(t)

	sink.fail = true
	if err := f.frames.Ingest(ctx, telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle,
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareFailed},
	}); err == nil {
		t.Fatal("the status write was made to fail, and the ingest reported no error")
	}
	sink.fail = false
	if st, why := f.confirm(t); st != fleetv1.RobotEstopResuming || !strings.Contains(why, "has not reached Robot.status") {
		t.Fatalf("ConfirmResume = %s (%q), want Resuming refused: the robot's latest report, lidar Failed, is not written", st, why)
	}

	// A delta frame that does not mention the lidar.
	pct := int32(80)
	if err := f.frames.Ingest(ctx, telemetry.Frame{
		RobotID: "amr-1", Namespace: f.ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle, BatteryPct: &pct,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if st, why := f.confirm(t); st != fleetv1.RobotEstopResuming || !strings.Contains(why, `"lidar" is reported Failed`) {
		t.Fatalf("ConfirmResume = %s (%q), want Resuming refused on the lidar Failed the next frame wrote", st, why)
	}

	f.report(t, fleetv1.HardwareHealthy)
	if st, why := f.confirm(t); st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal once the robot reports the lidar repaired", st, why)
	}
}

// failingStatusSink fails every status write while fail is set.
type failingStatusSink struct {
	inner telemetry.StatusSink
	fail  bool
}

func (s *failingStatusSink) ApplyMaterialUpdate(ctx context.Context, upd telemetry.MaterialUpdate) (bool, error) {
	if s.fail {
		return false, errors.New("apiserver unavailable")
	}
	return s.inner.ApplyMaterialUpdate(ctx, upd)
}

// A maintenance window paused the robot's pauseable capability before the stop. The
// capability was not Active at the stop, so it does not hold the release; the release
// happens, and the operator's close of the window, which waited for it, completes.
func TestEnvtest_EstopResume_AWindowOpenBeforeTheStopDoesNotDeadlock(t *testing.T) {
	requireEnvtest(t)
	f := newPreStopFixture(t, fleetv1.ClassCapability{Name: "transport", Type: fleetv1.CapabilityKindManual, Pauseable: true})
	zm := newEnvWindow(t, f.ns)
	driveEnvWindow(t, zm, f.ns)
	if got := getEnvRobot(t, f.ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s once the window is Active, want Maintenance", got)
	}
	f.rederive(t) // transport: Paused

	f.stopAndClear(t)
	closeEnvWindow(t, zm, f.ns)
	if got := envWindowPhase(t, f.ns); got != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("window phase = %s while the robot is Resuming, want Active: the close waits for the release", got)
	}

	f.report(t, fleetv1.HardwareHealthy)
	if st, why := f.confirm(t); st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal: transport was Paused, not Active, at the stop", st, why)
	}
	reconcileEnvWindow(t, zm, f.ns)
	if got := envWindowPhase(t, f.ns); got != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("window phase = %s after the release, want Completed", got)
	}
}

// A window opened while the robot is Resuming pauses a capability that was Active at
// the stop. The window's close waits for the release, so a paused capability cannot
// hold the release: it is the window's to restore, after the release.
func TestEnvtest_EstopResume_AWindowOpenedDuringResumingDoesNotDeadlock(t *testing.T) {
	requireEnvtest(t)
	f := newPreStopFixture(t, fleetv1.ClassCapability{Name: "transport", Type: fleetv1.CapabilityKindManual, Pauseable: true})
	f.stopAndClear(t)
	if b := getEnvRobot(t, f.ns, "amr-1").Status.EstopBaseline; b == nil || !slices.Equal(b.ActiveCapabilities, []string{"transport"}) {
		t.Fatalf("estopBaseline = %+v, want transport, Active at the stop", b)
	}

	zm := newEnvWindow(t, f.ns)
	driveEnvWindow(t, zm, f.ns)
	f.rederive(t) // transport: Paused
	closeEnvWindow(t, zm, f.ns)

	f.report(t, fleetv1.HardwareHealthy)
	if st, why := f.confirm(t); st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s (%q), want Normal: transport is Paused by the window whose close waits for this release", st, why)
	}
	reconcileEnvWindow(t, zm, f.ns)
	if got := envWindowPhase(t, f.ns); got != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("window phase = %s after the release, want Completed", got)
	}
}

func newEnvWindow(t *testing.T, ns string) *ZoneMaintenanceReconciler {
	t.Helper()
	zm := zoneMaint("m", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, Mode: fleetv1.ZoneMaintenanceModeGraceful,
		Reason: "conveyor service",
	})
	zm.Namespace = ns
	if err := envK8s.Create(context.Background(), zm); err != nil {
		t.Fatalf("create window: %v", err)
	}
	return &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme}
}

func reconcileEnvWindow(t *testing.T, r *ZoneMaintenanceReconciler, ns string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "m"}}); err != nil {
		t.Fatalf("reconcile window: %v", err)
	}
}

// driveEnvWindow adds the finalizer, then activates the window and pauses the robot.
func driveEnvWindow(t *testing.T, r *ZoneMaintenanceReconciler, ns string) {
	t.Helper()
	reconcileEnvWindow(t, r, ns)
	reconcileEnvWindow(t, r, ns)
}

// closeEnvWindow is the operator's close: spec.desiredState Completed, then a reconcile.
func closeEnvWindow(t *testing.T, r *ZoneMaintenanceReconciler, ns string) {
	t.Helper()
	ctx := context.Background()
	var zm fleetv1.ZoneMaintenance
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "m"}, &zm); err != nil {
		t.Fatalf("get window: %v", err)
	}
	zm.Spec.DesiredState = fleetv1.ZoneMaintenanceDesiredStateCompleted
	if err := envK8s.Update(ctx, &zm); err != nil {
		t.Fatalf("close window: %v", err)
	}
	reconcileEnvWindow(t, r, ns)
}

func envWindowPhase(t *testing.T, ns string) fleetv1.ZoneMaintenancePhase {
	t.Helper()
	var zm fleetv1.ZoneMaintenance
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "m"}, &zm); err != nil {
		t.Fatalf("get window: %v", err)
	}
	return zm.Status.Phase
}
