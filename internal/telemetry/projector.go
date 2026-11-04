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
	// Namespace is the namespace the adapter's ControlStream declared in its
	// AdapterHello, set by the ControlStream server. The robot-id annotation is unique
	// per namespace only (RFC-0001 #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), so a Robot is resolved by
	// this pair, never by RobotID alone. Empty when the adapter declared none; a
	// resolver then has no namespace to scope to and says so.
	Namespace string
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
// SwarmadaConfig (RFC-0001 #swarmadaconfig).
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
	// robot per rolling minute (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection). The more restrictive of this and
	// MinStatusWriteInterval applies; safety-critical changes bypass both. Zero
	// disables the ceiling.
	MaxStatusWritesPerMinute int
}

// ConfigFromTelemetry maps a SwarmadaConfig telemetry write-policy onto a projector
// [Config] (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection). It is the single translation from the CRD surface to the
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
	// Namespace is the frame's [Frame.Namespace], carried so the sink resolves
	// RobotID within it.
	Namespace string
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
	// several fields changed), used as the metrics-contract swarmada_telemetry_status_writes_total
	// transition_type label. One of the Transition* constants.
	TransitionType string
	// At is the time the projector decided to emit.
	At time.Time

	// undo is the robot's projector state from before this update was recorded, for
	// [Projector.Unsent].
	undo *undoRecord
}

// undoRecord restores a robot's projector state when the update that advanced it was not
// written.
type undoRecord struct {
	key  robotKey
	prev robotState
	// gen is the robot's state generation this update produced. A newer update for the
	// robot moves it on.
	gen uint64
}

// Transition* are the metrics-contract status-write transition_type label values, ordered by
// severity for the primary-reason classifier.
const (
	TransitionSafetyCritical   = "safety_critical"
	TransitionPhaseChange      = "phase_change"
	TransitionHardwareHealth   = "hardware_health_change"
	TransitionBatteryThreshold = "battery_threshold"
	TransitionAssignedAction   = "assigned_task_change"
)

// transitionType picks the single highest-severity reason a frame was material, so
// the status-write counter's total stays equal to the number of writes (RFC-0001 #control-plane-observability-prometheus-metrics-contract).
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
	// gen counts the updates emitted for this robot, so Unsent can tell whether the
	// update it is given is still the last one.
	gen uint64
	// unsent is set when an update's status write failed. The recorded state still
	// holds what the robot reported, and the next frame writes all of it (Unsent).
	// unsentCritical is whether that update bypassed the rate cap, and unsentType its
	// transition_type.
	unsent, unsentCritical bool
	unsentType             string
	// held is set while the rate cap holds a change the robot's latest frame reported.
	held bool
	// heldHW is the hardware the rate cap is holding. Hardware arrives as a delta and
	// the robot does not repeat it, so a held reading is kept here, not re-read from
	// the next frame, until it is written.
	heldHW map[string]fleetv1.HardwareStatus
}

// clone copies st, so a later change to st does not reach the copy.
func (st *robotState) clone() robotState {
	c := *st
	c.batteryPct = copyInt32(st.batteryPct)
	c.hardware = copyHWMap(st.hardware)
	c.heldHW = copyHWMap(st.heldHW)
	c.writeTimes = append([]time.Time(nil), st.writeTimes...)
	return c
}

// Projector turns a high-cadence telemetry stream into the rare material
// transitions that warrant a Robot.status write. It is safe for concurrent use.
type Projector struct {
	cfg   Config
	now   func() time.Time
	mu    sync.Mutex
	state map[robotKey]*robotState
}

// robotKey is what the projector remembers a robot by. RobotID alone is not enough:
// two namespaces may each hold a robot with the same robot_id (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), and a bare id routes both to the same plane, so keying on
// the id would let one robot's frames be compared against the other's last write.
type robotKey struct {
	namespace string
	robotID   string
}

func keyOf(f Frame) robotKey { return robotKey{namespace: f.Namespace, robotID: f.RobotID} }

// NewProjector builds a Projector. A nil BatteryThresholds falls back to the
// default boundaries; thresholds are sorted ascending.
func NewProjector(cfg Config) *Projector {
	if cfg.BatteryThresholds == nil {
		cfg.BatteryThresholds = DefaultConfig().BatteryThresholds
	}
	sorted := append([]int32(nil), cfg.BatteryThresholds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	cfg.BatteryThresholds = sorted
	return &Projector{cfg: cfg, now: time.Now, state: make(map[robotKey]*robotState)}
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
	st := p.ensure(keyOf(f))
	p.record(st, f)
	st.lastWrite = time.Time{}
}

// Forget drops robotID's recorded material state, so the next frame for that
// robot is treated as first sight and produces the one establishing status write
// [Projector.Project] emits for a never-before-seen robot. It reports whether an
// entry was actually dropped.
//
// namespace scopes the drop to the robot the caller means. The entry keyed by
// (namespace, robotID) is dropped, and so is the entry keyed by ("", robotID): a
// frame from a stream that declared no namespace records under "", and nothing on
// that entry says which namespace's robot it describes, so it cannot be kept for
// one robot and not another. Entries for robotID under any OTHER namespace are
// left alone — they belong to a different Robot with the same robot_id (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3). An empty
// namespace means the caller does not know it, and the id is dropped under every
// namespace.
//
// The recorded state is a cache of what was last written to THAT Robot object's
// status. When the object is deleted and recreated under the same wire robot_id
// the cache outlives the status it describes, and an unchanged telemetry stream —
// the sim fleet's battery never leaves its bucket, for instance — then matches the
// stale entry and emits nothing, leaving the recreated Robot with no
// telemetry-derived status at all. The owner of the robot_id -> object binding
// calls Forget when that binding changes; see
// controller.RobotProjectionResetter.
//
// RA-1: Forget performs no status write itself, but it CAUSES one: the robot's next
// frame is first sight, and produces the establishing write. That is its purpose
// after a recreation, and why a Forget must never reach a robot whose object did
// not change.
func (p *Projector) Forget(namespace, robotID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	forgotten := false
	for k := range p.state {
		if k.robotID != robotID {
			continue
		}
		if namespace != "" && k.namespace != namespace && k.namespace != "" {
			continue
		}
		delete(p.state, k)
		forgotten = true
	}
	return forgotten
}

// Project consumes one telemetry frame and returns a [MaterialUpdate] if and
// only if the frame is a material transition that should reach Robot.status
// (subject to the rate cap). It returns nil when nothing material changed — in
// particular, position-only churn always returns nil.
//
// The update is recorded as written when it is returned. A caller whose write of it
// fails calls [Projector.Unsent], or the next frame reporting the same reading is not
// material and the transition never reaches Robot.status.
func (p *Projector) Project(f Frame) *MaterialUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := keyOf(f)
	st := p.ensure(k)
	prev := st.clone()

	// First time we ever see this robot: establish its status in one write.
	if !st.initialized {
		p.record(st, f)
		st.lastWrite = p.now()
		return p.emitted(k, st, prev, p.initialUpdate(st, f))
	}

	incomingHW := f.Hardware
	if len(st.heldHW) > 0 {
		// A reading the cap held, overlaid by anything newer this frame reports.
		incomingHW = copyHWMap(st.heldHW)
		for name, status := range f.Hardware {
			incomingHW[name] = status
		}
	}
	hwChanged, mergedHW, hwCritical := mergeHardware(st.hardware, incomingHW)
	phaseChanged := f.Phase != "" && f.Phase != st.phase
	newBkt := batteryBucket(f.BatteryPct, p.cfg.BatteryThresholds)
	batteryChanged := newBkt != st.batteryBkt
	actionChanged := f.AssignedAction != st.assignedAction

	if !phaseChanged && !batteryChanged && !actionChanged && !hwChanged && !st.unsent {
		st.held, st.heldHW = false, nil
		return nil
	}

	critical := hwCritical ||
		(phaseChanged && (f.Phase == fleetv1.RobotPhaseOffline || f.Phase == fleetv1.RobotPhaseError)) ||
		(batteryChanged && newBkt == 0) ||
		(st.unsent && st.unsentCritical)

	// Rate cap: hold a non-critical change that arrives inside the window. We do
	// NOT advance recorded state, so the next frame re-evaluates and the change
	// flushes once the window elapses — it is delayed, never lost. Phase and battery
	// are in every frame; a hardware delta is not, so it is kept (hold).
	if !critical && p.cfg.MinStatusWriteInterval > 0 &&
		p.now().Sub(st.lastWrite) < p.cfg.MinStatusWriteInterval {
		st.hold(hwChanged, incomingHW)
		return nil
	}
	// Per-minute hard ceiling (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection): the more restrictive of this and the
	// interval cap applies. Prune the window to the trailing minute; hold a
	// non-critical change once the robot has hit its ceiling. Critical changes
	// bypass both caps and do not consume the budget.
	if !critical && p.cfg.MaxStatusWritesPerMinute > 0 {
		st.writeTimes = pruneBefore(st.writeTimes, p.now().Add(-time.Minute))
		if len(st.writeTimes) >= p.cfg.MaxStatusWritesPerMinute {
			st.hold(hwChanged, incomingHW)
			return nil
		}
	}

	upd := &MaterialUpdate{RobotID: f.RobotID, Namespace: f.Namespace, At: p.now()}
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
	if st.unsent {
		// A write failed since the last one that landed. This frame is folded in above;
		// write the whole recorded state, which holds the reading the failed write carried.
		upd = p.resendUpdate(st, f, upd)
	}
	st.unsent, st.unsentCritical, st.unsentType, st.held, st.heldHW = false, false, "", false, nil
	st.lastWrite = p.now()
	// Only non-critical writes consume the per-minute budget (critical writes are
	// exempt, matching the cap semantics above).
	if !critical {
		st.writeTimes = append(st.writeTimes, st.lastWrite)
	}
	return p.emitted(k, st, prev, upd)
}

// hold records that the rate cap is holding this frame's change, keeping the hardware
// delta it carries when that is part of the change.
func (st *robotState) hold(hwChanged bool, incomingHW map[string]fleetv1.HardwareStatus) {
	st.held = true
	st.heldHW = nil
	if hwChanged {
		st.heldHW = copyHWMap(incomingHW)
	}
}

// emitted attaches to upd what Unsent needs to take it back. The caller holds p.mu.
func (p *Projector) emitted(k robotKey, st *robotState, prev robotState, upd *MaterialUpdate) *MaterialUpdate {
	st.gen++
	upd.undo = &undoRecord{key: k, prev: prev, gen: st.gen}
	return upd
}

// Unsent records that upd, returned by Project, was not written to Robot.status. The
// robot's recorded state keeps what the robot reported, and is marked unsent: the next
// frame writes the whole recorded state, whether or not that frame changes anything. A
// failed write is delayed, never lost — including a hardware reading that arrived in a
// delta frame and is not reported again. The rate cap is restored, because the write it
// counted did not happen.
//
// If another update for the robot was emitted after upd, its rate-cap record is not
// upd's to restore, and only the mark is set. Either way nothing is written here, and
// the write that follows is the next frame's (RA-1: the same transition, written once).
func (p *Projector) Unsent(upd *MaterialUpdate) {
	if upd == nil || upd.undo == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[upd.undo.key]
	if !ok {
		return // forgotten since: the next frame is first sight anyway
	}
	if st.gen == upd.undo.gen {
		st.lastWrite, st.writeTimes = upd.undo.prev.lastWrite, upd.undo.prev.writeTimes
	}
	st.unsent = true
	if st.unsentType == "" || upd.TransitionType == TransitionSafetyCritical {
		st.unsentType = upd.TransitionType
	}
	st.unsentCritical = st.unsentCritical || upd.TransitionType == TransitionSafetyCritical
	st.gen++ // an Unsent of the same update again finds a newer generation
}

// Unwritten reports whether the robot's most recent frame carried a change that has not
// reached Robot.status: its status write failed and has not been made since, or the rate
// cap is holding it. A robot the projector holds nothing for has had nothing written
// from its reports, and is unwritten.
func (p *Projector) Unwritten(namespace, robotID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[robotKey{namespace: namespace, robotID: robotID}]
	if !ok || !st.initialized {
		return true
	}
	return st.unsent || st.held
}

// pruneBefore drops timestamps at or before cutoff, in place, preserving order.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && !times[i].After(cutoff) {
		i++
	}
	return times[i:]
}

// resendUpdate is the write that follows a failed one: the robot's whole recorded state,
// as the establishing write carries it, labelled with this frame's transition or, when
// the frame changed nothing, the failed write's. upd is this frame's own update.
func (p *Projector) resendUpdate(st *robotState, f Frame, upd *MaterialUpdate) *MaterialUpdate {
	full := &MaterialUpdate{
		RobotID: f.RobotID, Namespace: f.Namespace, At: upd.At,
		Reason: "resend after a failed status write", TransitionType: upd.TransitionType,
	}
	if full.TransitionType == "" {
		full.TransitionType = st.unsentType
	}
	if st.phase != "" {
		ph := st.phase
		full.Phase = &ph
	}
	full.BatteryPct = copyInt32(st.batteryPct)
	if len(st.hardware) > 0 {
		full.Hardware = copyHWMap(st.hardware)
	}
	action := st.assignedAction
	full.AssignedAction = &action
	return full
}

// initialUpdate builds the one-shot establishing write for a never-before-seen
// robot. st has already been populated by record.
func (p *Projector) initialUpdate(st *robotState, f Frame) *MaterialUpdate {
	// The establishing write is classified as a phase_change: it materialises the
	// robot's lifecycle phase for the first time.
	upd := &MaterialUpdate{
		RobotID: f.RobotID, Namespace: f.Namespace, At: p.now(),
		Reason: "initial telemetry", TransitionType: TransitionPhaseChange,
	}
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

func (p *Projector) ensure(k robotKey) *robotState {
	st, ok := p.state[k]
	if !ok {
		st = &robotState{}
		p.state[k] = st
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
