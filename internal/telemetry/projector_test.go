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

package telemetry_test

import (
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

const testRobot = "warehouse-east/acme-bot-042"

func i32(v int32) *int32 { return &v }

// frame builds a telemetry frame; hw is the delta component set for this frame.
func frame(
	phase fleetv1.RobotPhase,
	battery *int32,
	action string,
	hw map[string]fleetv1.HardwareStatus,
) telemetry.Frame {
	return telemetry.Frame{
		RobotID:        testRobot,
		Timestamp:      time.Now(),
		Phase:          phase,
		BatteryPct:     battery,
		Hardware:       hw,
		AssignedAction: action,
	}
}

func baseFrame() telemetry.Frame {
	return frame(fleetv1.RobotPhaseIdle, i32(80), "", map[string]fleetv1.HardwareStatus{
		"lidar-main": fleetv1.HardwareHealthy,
	})
}

// TestNoMaterialChangeProducesNoWrites is the load-bearing RA-1 assertion: a
// primed projector fed an unchanging telemetry stream writes nothing to status.
func TestNoMaterialChangeProducesNoWrites(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	base := baseFrame()
	p.Prime(base)

	writes := 0
	for i := 0; i < 50; i++ {
		if p.Project(base) != nil {
			writes++
		}
	}
	if writes != 0 {
		t.Fatalf("expected 0 status writes for unchanged stream, got %d", writes)
	}
}

// TestPositionChurnIsIgnored proves live pose never drives a status write — it
// belongs to the TSDB plane only (RA-1).
func TestPositionChurnIsIgnored(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	p.Prime(baseFrame())

	writes := 0
	for i := 0; i < 20; i++ {
		f := baseFrame()
		f.Position = &telemetry.Position{X: float64(i), Y: float64(2 * i), Yaw: 0.1 * float64(i)}
		if p.Project(f) != nil {
			writes++
		}
	}
	if writes != 0 {
		t.Fatalf("position churn must not write status, got %d writes", writes)
	}
}

// TestInitialFrameEstablishesStatus checks an unprimed robot's first frame
// produces exactly one establishing write.
func TestInitialFrameEstablishesStatus(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	if upd := p.Project(baseFrame()); upd == nil {
		t.Fatal("first-ever frame should establish status, got nil")
	}
	if upd := p.Project(baseFrame()); upd != nil {
		t.Fatalf("second identical frame should be a no-op, got %+v", upd)
	}
}

// TestPhaseChangeProducesOneWrite checks a phase transition writes once.
func TestPhaseChangeProducesOneWrite(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	p.Prime(baseFrame())

	upd := p.Project(frame(fleetv1.RobotPhaseInProgress, i32(80), "", nil))
	if upd == nil || upd.Phase == nil || *upd.Phase != fleetv1.RobotPhaseInProgress {
		t.Fatalf("expected an InProgress phase write, got %+v", upd)
	}
	if again := p.Project(frame(fleetv1.RobotPhaseInProgress, i32(80), "", nil)); again != nil {
		t.Fatalf("repeat of same phase should be a no-op, got %+v", again)
	}
}

// TestBatteryCrossesBucketBoundary checks writes happen on bucket crossings, not
// on every percent, and that entering the lowest bucket still writes.
func TestBatteryCrossesBucketBoundary(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig()) // thresholds 15, 30
	p.Prime(baseFrame())                                   // 80 -> bucket 2

	if upd := p.Project(frame(fleetv1.RobotPhaseIdle, i32(25), "", nil)); upd == nil {
		t.Fatal("80->25 crosses the 30 boundary and must write")
	}
	if upd := p.Project(frame(fleetv1.RobotPhaseIdle, i32(24), "", nil)); upd != nil {
		t.Fatalf("25->24 stays in the same bucket and must not write, got %+v", upd)
	}
	if upd := p.Project(frame(fleetv1.RobotPhaseIdle, i32(10), "", nil)); upd == nil {
		t.Fatal("24->10 enters the critical bucket and must write")
	}
}

// TestHardwareTransitionWrites checks a delta component status change writes once
// and reports the full resolved map.
func TestHardwareTransitionWrites(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	p.Prime(baseFrame()) // lidar-main Healthy

	upd := p.Project(frame(fleetv1.RobotPhaseIdle, i32(80), "", map[string]fleetv1.HardwareStatus{
		"lidar-main": fleetv1.HardwareDegraded,
	}))
	if upd == nil || upd.Hardware["lidar-main"] != fleetv1.HardwareDegraded {
		t.Fatalf("expected lidar-main Degraded in the write, got %+v", upd)
	}
	if again := p.Project(frame(fleetv1.RobotPhaseIdle, i32(80), "", map[string]fleetv1.HardwareStatus{
		"lidar-main": fleetv1.HardwareDegraded,
	})); again != nil {
		t.Fatalf("repeat of same hardware status should be a no-op, got %+v", again)
	}
}

// TestAssignedActionChangeWrites checks an assigned-action change writes once.
func TestAssignedActionChangeWrites(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	p.Prime(baseFrame())

	upd := p.Project(frame(fleetv1.RobotPhaseInProgress, i32(80), "pick-1", nil))
	if upd == nil || upd.AssignedAction == nil || *upd.AssignedAction != "pick-1" {
		t.Fatalf("expected assignedAction=pick-1 in the write, got %+v", upd)
	}
}

// TestRateCapHoldsNonCriticalButPassesCritical exercises the per-robot write cap
// with an injected clock: a non-critical change inside the window is held (and
// later flushed), while a safety-critical change bypasses the cap.
func TestRateCapHoldsNonCriticalButPassesCritical(t *testing.T) {
	var now time.Time
	clock := func() time.Time { return now }
	cfg := telemetry.Config{
		MinStatusWriteInterval: 10 * time.Second,
		BatteryThresholds:      []int32{15, 30},
	}
	p := telemetry.NewProjectorWithClock(cfg, clock)
	p.Prime(baseFrame())

	now = time.Unix(100, 0)
	if p.Project(frame(fleetv1.RobotPhaseIdle, i32(80), "task-a", nil)) == nil {
		t.Fatal("first material change after prime should write")
	}

	now = time.Unix(105, 0) // 5s < 10s window, non-critical -> held
	if upd := p.Project(frame(fleetv1.RobotPhaseIdle, i32(80), "task-b", nil)); upd != nil {
		t.Fatalf("non-critical change inside the cap window should be held, got %+v", upd)
	}

	now = time.Unix(106, 0) // critical phase change bypasses the cap
	if p.Project(frame(fleetv1.RobotPhaseOffline, i32(80), "task-a", nil)) == nil {
		t.Fatal("critical (Offline) change must bypass the rate cap")
	}

	now = time.Unix(120, 0) // window elapsed -> the still-pending action change flushes
	if p.Project(frame(fleetv1.RobotPhaseOffline, i32(80), "task-b", nil)) == nil {
		t.Fatal("held change should flush once the cap window elapses")
	}
}

// TestForgetReestablishesStatusForARecreatedRobot is the regression assertion for
// the defect Projector.Forget exists to fix: a Robot deleted and recreated under the
// same wire robot_id starts with an EMPTY status, but the projector's per-robot cache
// is keyed by robot_id alone — no UID, no object identity — so it outlives the object
// and suppresses the re-establishing write, and the recreated robot never gets
// batteryPercent or hardware back for as long as the manager process runs.
//
// The battery reading deliberately does not move between the two generations. That
// is the real case, not a contrived one: the reference sim adapter starts every
// robot at 100% and drains 0.01%/s, so across a demo-length run the reading never
// leaves its bucket and NOTHING about the stream is material to the stale cache.
func TestForgetReestablishesStatusForARecreatedRobot(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())

	// First generation of the Robot object: one establishing write, then quiet.
	if upd := p.Project(baseFrame()); upd == nil || upd.BatteryPct == nil {
		t.Fatalf("first-ever frame should establish battery, got %+v", upd)
	}
	if upd := p.Project(baseFrame()); upd != nil {
		t.Fatalf("unchanged stream should be a no-op, got %+v", upd)
	}

	// The Robot object is deleted and recreated with an empty status. Without the
	// reset, the identical stream stays a no-op and status is never re-established.
	if upd := p.Project(baseFrame()); upd != nil {
		t.Fatalf("stale-cache precondition broken: expected no-op, got %+v", upd)
	}

	if !p.Forget("", testRobot) {
		t.Fatal("Forget reported nothing to drop for a robot the projector had recorded")
	}

	upd := p.Project(baseFrame())
	if upd == nil {
		t.Fatal("after Forget, the next frame must re-establish status; got nil")
	}
	if upd.BatteryPct == nil || *upd.BatteryPct != 80 {
		t.Fatalf("re-established update must carry the battery reading, got %+v", upd.BatteryPct)
	}
	if upd.Hardware["lidar-main"] != fleetv1.HardwareHealthy {
		t.Fatalf("re-established update must carry hardware, got %+v", upd.Hardware)
	}

	// RA-1: re-establishing is ONE write, not a per-tick unlock. The stream is
	// unchanged, so every following frame must again be a no-op.
	for i := 0; i < 20; i++ {
		if again := p.Project(baseFrame()); again != nil {
			t.Fatalf("frame %d after the establishing write must be a no-op, got %+v", i, again)
		}
	}
}

// TestForgetUnknownRobotIsANoOp checks Forget reports honestly rather than
// creating an entry for a robot it has never seen.
func TestForgetUnknownRobotIsANoOp(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	if p.Forget("", "warehouse-east/never-seen") {
		t.Fatal("Forget claimed to drop state for a robot the projector never recorded")
	}
	if upd := p.Project(baseFrame()); upd == nil {
		t.Fatal("a Forget of another robot must not consume this robot's first frame")
	}
}
