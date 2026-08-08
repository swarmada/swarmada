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

package telemetry

import (
	"sort"
	"strings"
	"sync"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Frame is one normalized telemetry sample for a single robot, already
// translated from the wire TelemetryPayload (see the package doc for the
// mapping). It is deliberately free of any proto dependency so the projection
// logic is trivially unit-testable.
type Frame struct {
	// RobotID is the namespace/name (or stable robot id) the frame belongs to.
	RobotID string
	// Adapter is the mTLS-verified FleetAdapter that delivered this frame, set by
	// the ControlStream server (empty in dev mode without a verified identity). It
	// is used only as a metric label (swarmada_telemetry_dropped_frames_total), never
	// for a material status decision (RA-1).
	Adapter string
	// Timestamp is when the reading was taken (from TelemetryPayload.timestamp_ms).
	Timestamp time.Time

	// Phase is the CRD-level lifecycle phase. Empty means "unreported"; the
	// projector leaves the recorded phase unchanged rather than clobbering it.
	Phase fleetv1.RobotPhase
	// BatteryPct is the last reported charge (0-100), or nil if unreported. A
	// pointer because proto3 explicit presence makes 0 a valid critical reading
	// distinct from "absent".
	BatteryPct *int32
	// Position is the last pose. It is recorded to the TSDB only and never drives
	// a material status write (RA-1).
	Position *Position
	// Hardware is the DELTA set of component->status from this frame (all
	// components on the first frame after reconnect). The projector merges it onto
	// the last-known full map.
	Hardware map[string]fleetv1.HardwareStatus
	// AssignedAction is the action the robot reports executing, empty if idle.
	AssignedAction string
}

// Position is a robot pose in the zone coordinate frame. It exists for the TSDB
// path; it is intentionally not part of [MaterialUpdate].
type Position struct {
	X     float64
	Y     float64
	Yaw   float64
	Floor int32
}

// Samples flattens the high-cadence fields of a frame into TSDB samples. This is
// the data that goes to the time-series store every tick — never to etcd.
func (f Frame) Samples() []Sample {
	labels := map[string]string{"robot_id": f.RobotID}
	var out []Sample
	add := func(metric string, v float64) {
		out = append(out, Sample{
			RobotID:   f.RobotID,
			Timestamp: f.Timestamp,
			Metric:    metric,
			Value:     v,
			Labels:    labels,
		})
	}
	if f.Position != nil {
		add("robot_position_x_meters", f.Position.X)
		add("robot_position_y_meters", f.Position.Y)
		add("robot_position_yaw_radians", f.Position.Yaw)
		add("robot_floor", float64(f.Position.Floor))
	}
	if f.BatteryPct != nil {
		add("robot_battery_percent", float64(*f.BatteryPct))
	}
	return out
}

// Config tunes the status projection. The zero value is usable (battery
// thresholds default, rate cap disabled); production values come from
// SwarmadaConfig (RFC-0001 §5.2.11).
type Config struct {
	// MinStatusWriteInterval caps how often a single robot's status may be
	// written. A non-critical material change arriving sooner is held until the
	// interval elapses; safety-critical changes (Offline/Degraded, critical
	// battery, hardware fault) always bypass the cap. Zero disables the cap, so
	// every material change is written immediately.
	MinStatusWriteInterval time.Duration

	// BatteryThresholds are the ascending percentage boundaries that bucket the
	// battery reading. A status write occurs only when the reading crosses a
	// boundary, not on every percent. Entering the lowest bucket is critical.
	BatteryThresholds []int32

	// MaxStatusWritesPerMinute is a hard ceiling on NON-critical status writes per
	// robot per rolling minute (§9.1.11.7). The more restrictive of this and
	// MinStatusWriteInterval applies; safety-critical changes bypass both. Zero
	// disables the ceiling.
	MaxStatusWritesPerMinute int
}

// ConfigFromTelemetry maps a SwarmadaConfig telemetry write-policy onto a projector
// [Config] (§9.1.11.7). It is the single translation from the CRD surface to the
// projector's throttle knobs.
func ConfigFromTelemetry(minWriteIntervalSeconds, maxWritesPerMinute int32, batteryThresholds []int32) Config {
	thresholds := batteryThresholds
	if len(thresholds) == 0 {
		thresholds = DefaultConfig().BatteryThresholds
	}
	return Config{
		MinStatusWriteInterval:   time.Duration(minWriteIntervalSeconds) * time.Second,
		BatteryThresholds:        thresholds,
		MaxStatusWritesPerMinute: int(maxWritesPerMinute),
	}
}

// DefaultConfig returns the built-in defaults: battery boundaries at 15% and 30%
// and no rate cap (every material change is written). A deployment that wants to
// bound status churn sets MinStatusWriteInterval via SwarmadaConfig.
func DefaultConfig() Config {
	return Config{
		MinStatusWriteInterval: 0,
		BatteryThresholds:      []int32{15, 30},
	}
}

// MaterialUpdate is the throttled, material-only projection the [Projector]
// hands to a [StatusSink] for writing to Robot.status. Only changed fields are
// non-nil. Position is deliberately absent: live pose is served from the TSDB,
// never written to etcd (RA-1).
type MaterialUpdate struct {
	// RobotID identifies the robot whose status should be patched.
	RobotID string
	// Phase, when non-nil, is the new lifecycle phase to write.
	Phase *fleetv1.RobotPhase
	// BatteryPct, when non-nil, is the new (bucket-crossing) battery reading.
	BatteryPct *int32
	// Hardware, when non-nil, is the full current component->status map to write
	// (the projector resolves deltas into a complete map before emitting).
	Hardware map[string]fleetv1.HardwareStatus
	// AssignedAction, when non-nil, is the new assigned-action value to write.
	AssignedAction *string
	// Reason is a short human-readable description of what made the frame material.
	Reason string
	// TransitionType is the machine-readable primary reason (highest severity when
	// several fields changed), used as the §9.3.8 swarmada_telemetry_status_writes_total
	// transition_type label. One of the Transition* constants.
	TransitionType string
	// At is the time the projector decided to emit.
	At time.Time
}

// Transition* are the §9.3.8 status-write transition_type label values, ordered by
// severity for the primary-reason classifier.
const (
	TransitionSafetyCritical   = "safety_critical"
	TransitionPhaseChange      = "phase_change"
	TransitionHardwareHealth   = "hardware_health_change"
	TransitionBatteryThreshold = "battery_threshold"
	TransitionAssignedAction   = "assigned_task_change"
)

// transitionType picks the single highest-severity reason a frame was material, so
// the status-write counter's total stays equal to the number of writes (§9.3.8).
func transitionType(critical, phase, hw, battery, action bool) string {
	switch {
	case critical:
		return TransitionSafetyCritical
	case phase:
		return TransitionPhaseChange
	case hw:
		return TransitionHardwareHealth
	case battery:
		return TransitionBatteryThreshold
	case action:
		return TransitionAssignedAction
	default:
		return ""
	}
}

// robotState is the projector's per-robot memory of the last-written material
// state, plus the rate-cap clock.
type robotState struct {
	initialized    bool
	phase          fleetv1.RobotPhase
	batteryPct     *int32
	batteryBkt     int
	hardware       map[string]fleetv1.HardwareStatus
	assignedAction string
	lastWrite      time.Time
	// writeTimes are the times of recent NON-critical status writes, kept for the
	// per-minute ceiling (pruned to the trailing minute on each check).
	writeTimes []time.Time
}

// Projector turns a high-cadence telemetry stream into the rare material
// transitions that warrant a Robot.status write. It is safe for concurrent use.
type Projector struct {
	cfg   Config
	now   func() time.Time
	mu    sync.Mutex
	state map[string]*robotState
}

// NewProjector builds a Projector. A nil BatteryThresholds falls back to the
// default boundaries; thresholds are sorted ascending.
func NewProjector(cfg Config) *Projector {
	if cfg.BatteryThresholds == nil {
		cfg.BatteryThresholds = DefaultConfig().BatteryThresholds
	}
	sorted := append([]int32(nil), cfg.BatteryThresholds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	cfg.BatteryThresholds = sorted
	return &Projector{cfg: cfg, now: time.Now, state: make(map[string]*robotState)}
}

// NewProjectorWithClock is like [NewProjector] but uses an injectable clock,
// allowing the rate cap to be exercised deterministically in tests.
func NewProjectorWithClock(cfg Config, now func() time.Time) *Projector {
	p := NewProjector(cfg)
	p.now = now
	return p
}

// SetConfig replaces the projection policy in place, preserving each robot's
// recorded material state and rate-cap history so a reconfiguration never forces a
// spurious re-establishing write. Battery thresholds are re-sorted; a nil set falls
// back to the defaults. Safe for concurrent use.
func (p *Projector) SetConfig(cfg Config) {
	if cfg.BatteryThresholds == nil {
		cfg.BatteryThresholds = DefaultConfig().BatteryThresholds
	}
	sorted := append([]int32(nil), cfg.BatteryThresholds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	cfg.BatteryThresholds = sorted
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Prime records a robot's current material state WITHOUT emitting an update, so
// that a subsequent unchanged telemetry stream produces zero status writes. A
// controller calls Prime from the robot's existing Status on cache sync.
func (p *Projector) Prime(f Frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.ensure(f.RobotID)
	p.record(st, f)
	st.lastWrite = time.Time{}
}

// Project consumes one telemetry frame and returns a [MaterialUpdate] if and
// only if the frame is a material transition that should reach Robot.status
// (subject to the rate cap). It returns nil when nothing material changed — in
// particular, position-only churn always returns nil.
func (p *Projector) Project(f Frame) *MaterialUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.ensure(f.RobotID)

	// First time we ever see this robot: establish its status in one write.
	if !st.initialized {
		p.record(st, f)
		st.lastWrite = p.now()
		return p.initialUpdate(st, f)
	}

	hwChanged, mergedHW, hwCritical := mergeHardware(st.hardware, f.Hardware)
	phaseChanged := f.Phase != "" && f.Phase != st.phase
	newBkt := batteryBucket(f.BatteryPct, p.cfg.BatteryThresholds)
	batteryChanged := newBkt != st.batteryBkt
	actionChanged := f.AssignedAction != st.assignedAction

	if !phaseChanged && !batteryChanged && !actionChanged && !hwChanged {
		return nil
	}

	critical := hwCritical ||
		(phaseChanged && (f.Phase == fleetv1.RobotPhaseOffline || f.Phase == fleetv1.RobotPhaseError)) ||
		(batteryChanged && newBkt == 0)

	// Rate cap: hold a non-critical change that arrives inside the window. We do
	// NOT advance recorded state, so the next frame re-evaluates and the change
	// flushes once the window elapses — it is delayed, never lost.
	if !critical && p.cfg.MinStatusWriteInterval > 0 &&
		p.now().Sub(st.lastWrite) < p.cfg.MinStatusWriteInterval {
		return nil
	}
	// Per-minute hard ceiling (§9.1.11.7): the more restrictive of this and the
	// interval cap applies. Prune the window to the trailing minute; hold a
	// non-critical change once the robot has hit its ceiling. Critical changes
	// bypass both caps and do not consume the budget.
	if !critical && p.cfg.MaxStatusWritesPerMinute > 0 {
		st.writeTimes = pruneBefore(st.writeTimes, p.now().Add(-time.Minute))
		if len(st.writeTimes) >= p.cfg.MaxStatusWritesPerMinute {
			return nil
		}
	}

	upd := &MaterialUpdate{RobotID: f.RobotID, At: p.now()}
	if phaseChanged {
		ph := f.Phase
		upd.Phase = &ph
		st.phase = ph
	}
	if batteryChanged {
		upd.BatteryPct = copyInt32(f.BatteryPct)
		st.batteryPct = copyInt32(f.BatteryPct)
		st.batteryBkt = newBkt
	}
	if hwChanged {
		st.hardware = mergedHW
		upd.Hardware = copyHWMap(mergedHW)
	}
	if actionChanged {
		t := f.AssignedAction
		upd.AssignedAction = &t
		st.assignedAction = t
	}
	upd.Reason = reasonString(phaseChanged, batteryChanged, hwChanged, actionChanged)
	upd.TransitionType = transitionType(critical, phaseChanged, hwChanged, batteryChanged, actionChanged)
	st.lastWrite = p.now()
	// Only non-critical writes consume the per-minute budget (critical writes are
	// exempt, matching the cap semantics above).
	if !critical {
		st.writeTimes = append(st.writeTimes, st.lastWrite)
	}
	return upd
}

// pruneBefore drops timestamps at or before cutoff, in place, preserving order.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	return times[i:]
}

// initialUpdate builds the one-shot establishing write for a never-before-seen
// robot. st has already been populated by record.
func (p *Projector) initialUpdate(st *robotState, f Frame) *MaterialUpdate {
	// The establishing write is classified as a phase_change: it materialises the
	// robot's lifecycle phase for the first time.
	upd := &MaterialUpdate{RobotID: f.RobotID, At: p.now(), Reason: "initial telemetry", TransitionType: TransitionPhaseChange}
	if f.Phase != "" {
		ph := st.phase
		upd.Phase = &ph
	}
	upd.BatteryPct = copyInt32(st.batteryPct)
	if len(st.hardware) > 0 {
		upd.Hardware = copyHWMap(st.hardware)
	}
	action := st.assignedAction
	upd.AssignedAction = &action
	return upd
}

// record folds a frame into the recorded state without emitting.
func (p *Projector) record(st *robotState, f Frame) {
	st.initialized = true
	if f.Phase != "" {
		st.phase = f.Phase
	}
	st.batteryPct = copyInt32(f.BatteryPct)
	st.batteryBkt = batteryBucket(f.BatteryPct, p.cfg.BatteryThresholds)
	_, merged, _ := mergeHardware(st.hardware, f.Hardware)
	st.hardware = merged
	st.assignedAction = f.AssignedAction
}

func (p *Projector) ensure(robotID string) *robotState {
	st, ok := p.state[robotID]
	if !ok {
		st = &robotState{}
		p.state[robotID] = st
	}
	return st
}

// batteryBucket returns the index of the lowest ascending threshold the reading
// falls below, or len(thresholds) when above them all. A nil reading is bucket
// -1 ("unknown"), so a known<->unknown transition is itself material.
func batteryBucket(pct *int32, thresholds []int32) int {
	if pct == nil {
		return -1
	}
	b := 0
	for _, t := range thresholds {
		if *pct < t {
			return b
		}
		b++
	}
	return b
}

// mergeHardware folds a delta set onto a copy of the previous full map. It
// reports whether any component's status changed and whether any change is a
// transition into a degraded/absent (critical) state. It never mutates old.
func mergeHardware(
	old, delta map[string]fleetv1.HardwareStatus,
) (changed bool, merged map[string]fleetv1.HardwareStatus, critical bool) {
	merged = make(map[string]fleetv1.HardwareStatus, len(old)+len(delta))
	for k, v := range old {
		merged[k] = v
	}
	for name, status := range delta {
		prev, existed := merged[name]
		if !existed || prev != status {
			changed = true
			if status == fleetv1.HardwareDegraded || status == fleetv1.HardwareFailed {
				critical = true
			}
		}
		merged[name] = status
	}
	return changed, merged, critical
}

func reasonString(phase, battery, hw, action bool) string {
	parts := make([]string, 0, 4)
	if phase {
		parts = append(parts, "phase")
	}
	if battery {
		parts = append(parts, "battery")
	}
	if hw {
		parts = append(parts, "hardware")
	}
	if action {
		parts = append(parts, "task")
	}
	return "material change: " + strings.Join(parts, ",")
}

func copyInt32(v *int32) *int32 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyHWMap(m map[string]fleetv1.HardwareStatus) map[string]fleetv1.HardwareStatus {
	if m == nil {
		return nil
	}
	out := make(map[string]fleetv1.HardwareStatus, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
