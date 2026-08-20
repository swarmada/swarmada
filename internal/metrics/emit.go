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

package metrics

import "time"

// EstopScope is the `scope` label on the estop metrics (§9.3.8): which operator
// action produced the stop, not which object was written. A typed value rather than
// a bare string because every emit site previously hard-coded ScopeRobot — a zone or
// namespace estop fans out per robot through the same dispatcher, so nothing in the
// robot-level code path can tell the three apart and the label has to be carried in.
type EstopScope string

// Estop scope label values (§9.3.8 swarmada_estop_command* `scope`).
const (
	ScopeRobot     EstopScope = "robot"
	ScopeZone      EstopScope = "zone"
	ScopeNamespace EstopScope = "namespace"
)

// Estop command result label values (§9.3.8 swarmada_estop_commands_total `result`).
const (
	ResultAckStopping = "ack_stopping"
	ResultAckStopped  = "ack_stopped"
	ResultAckFailed   = "ack_failed"
	ResultTimeout     = "timeout"
)

// ObserveEstopLatency records an estop send→EstopAck round-trip (§9.3.8).
//
// This is a PER-ROBOT round trip and, for a fan-out, it is not the number that
// matters: sequential dispatch delays each robot's SEND, so every robot in a
// 50-robot zone estop can report a healthy sub-SLA round trip while the last one is
// commanded tens of seconds after the operator hit the trigger. ObserveEstopFanout
// measures that interval; see ADR-0042.
func ObserveEstopLatency(namespace, adapter, robot string, scope EstopScope, d time.Duration) {
	EstopCommandLatencySeconds.WithLabelValues(namespace, adapter, robot, string(scope)).Observe(d.Seconds())
}

// ObserveEstopFanout records one zone- or namespace-scoped estop episode end to end:
// from the operator's trigger to the last robot in scope resolving (§9.6.2.2).
//
// Observed ONCE per episode, by the fanning-out controller — not per robot. A
// robot-scoped estop has no fan-out and must not observe it, or the histogram's
// population stops being "episodes that fan out" and its quantiles become
// meaningless.
func ObserveEstopFanout(namespace string, scope EstopScope, d time.Duration) {
	EstopFanoutDurationSeconds.WithLabelValues(namespace, string(scope)).Observe(d.Seconds())
}

// IncEstopLatencyViolation records an estop ACK that breached the 500ms SLA (§9.3.8).
func IncEstopLatencyViolation(namespace, adapter, robot string) {
	EstopLatencyViolationsTotal.WithLabelValues(namespace, adapter, robot).Inc()
}

// IncEstopCommand records one issued estop Command by terminal disposition (§9.3.8).
func IncEstopCommand(namespace, adapter string, scope EstopScope, result string) {
	EstopCommandsTotal.WithLabelValues(namespace, adapter, string(scope), result).Inc()
}

// ── Telemetry pipeline emit helpers (§9.3.8) ──────────────────────────────────

// IncTelemetryFramesReceived records one TelemetryPayload frame received on
// ControlStream (§9.3.8, the write-amplification denominator).
func IncTelemetryFramesReceived(namespace, adapter string) {
	TelemetryFramesReceivedTotal.WithLabelValues(namespace, adapter).Inc()
}

// IncTelemetryStatusWrite records one Robot.status write the projector emitted on
// a material transition (§9.3.8). transitionType: phase_change,
// hardware_health_change, battery_threshold, assigned_action_change, safety_critical.
func IncTelemetryStatusWrite(namespace, transitionType string) {
	TelemetryStatusWritesTotal.WithLabelValues(namespace, transitionType).Inc()
}

// IncTSDBWriteError records one TSDBWriter write failure (§9.3.8). error_class:
// timeout, auth, schema, unknown; sink_type is the configured store.
func IncTSDBWriteError(namespace, sinkType, errorClass string) {
	TelemetryTSDBWriteErrorsTotal.WithLabelValues(namespace, sinkType, errorClass).Inc()
}

// IncTelemetryDroppedFrame records one telemetry frame not forwardable to the TSDB
// sink (§9.3.8). It never fires when the sink is Drop/unset (NoopWriter never errors).
func IncTelemetryDroppedFrame(namespace, adapter string) {
	TelemetryDroppedFramesTotal.WithLabelValues(namespace, adapter).Inc()
}

// ── Resource-count gauge setters (§9.3.8) ─────────────────────────────────────
// Each writes the FULL enum label set for the key, so a phase/state with no
// resources reads 0 rather than absent (§9.3.8 registration requirement) and a
// value that emptied since the last set is de-staled to 0 within a present key.

// SetFleetActionsByPhase sets swarmada_fleetactions_by_phase for a namespace from a
// phase→count map (missing phases → 0).
func SetFleetActionsByPhase(namespace string, counts map[string]int) {
	for _, p := range FleetActionPhases {
		FleetActionsByPhase.WithLabelValues(namespace, p).Set(float64(counts[p]))
	}
}

// SetRobotsByPhase sets swarmada_robots_by_phase for a namespace (missing → 0).
func SetRobotsByPhase(namespace string, counts map[string]int) {
	for _, p := range RobotPhases {
		RobotsByPhase.WithLabelValues(namespace, p).Set(float64(counts[p]))
	}
}

// SetRobotsInEstop sets swarmada_robots_in_estop for a namespace over the active
// estop states (Stopping, Stopped, Failed; Normal is excluded per §9.3.8).
//
// Failed is an ACTIVE state for this gauge's purpose: a robot whose estop could not be
// confirmed is withheld from dispatch and is exactly what an operator is looking for.
// The sweeper has always counted it; until EstopStates listed it the count was computed
// every sweep and discarded here.
func SetRobotsInEstop(namespace string, counts map[string]int) {
	for _, s := range EstopStates {
		RobotsInEstop.WithLabelValues(namespace, s).Set(float64(counts[s]))
	}
}

// ── Scheduler & assignment-lease emit helpers (§9.3.8) ────────────────────────

// Assignment-failure reasons (§9.3.8 swarmada_scheduler_assignment_failures_total).
const (
	FailureDeadlineExceeded           = "DeadlineExceeded"
	FailureNoCandidates               = "NoCandidates"
	FailureAdapterDisconnected        = "AdapterDisconnected"
	FailureAllCandidatesInEstop       = "AllCandidatesInEstop"
	FailureAllCandidatesInMaintenance = "AllCandidatesInMaintenance"
)

// ObserveAssignmentLatency records the time from a FleetAction entering Pending to
// its Assigned transition (§9.3.8). priority: Critical, High, Normal, Low.
func ObserveAssignmentLatency(namespace, priority string, d time.Duration) {
	SchedulerAssignmentLatencySeconds.WithLabelValues(namespace, priority).Observe(d.Seconds())
}

// IncLeaseRenewal records one successful assignment-lease renewal (§9.3.8).
func IncLeaseRenewal(namespace string) {
	SchedulerLeaseRenewalsTotal.WithLabelValues(namespace).Inc()
}

// IncLeaseExpiry records one lease that expired past the horizon, driving a
// Revoking→Pending reassignment (§9.3.8 — the robot self-stopped its action).
func IncLeaseExpiry(namespace string) {
	SchedulerLeaseExpiriesTotal.WithLabelValues(namespace).Inc()
}

// IncAssignmentFailure records an assignment ending Failed or abandoned (§9.3.8).
func IncAssignmentFailure(namespace, reason string) {
	SchedulerAssignmentFailuresTotal.WithLabelValues(namespace, reason).Inc()
}

// IncAdapterReconnect records one FleetAdapter ControlStream re-establishment
// after a Disconnected/Degraded phase (§9.3.8).
func IncAdapterReconnect(namespace, adapter string) {
	FleetAdapterReconnectsTotal.WithLabelValues(namespace, adapter).Inc()
}

// ObserveRobotOfflineDuration records a completed Offline span, observed at
// reconnect (§9.3.8 — a completion-time metric, not a live-staleness gauge).
func ObserveRobotOfflineDuration(namespace string, d time.Duration) {
	RobotOfflineDurationSeconds.WithLabelValues(namespace).Observe(d.Seconds())
}

// IncRobotConnectivityCritical counts one escalation to the ConnectivityCritical
// condition for a namespace (the Offline→beyond-critical-threshold edge, ADR-0011).
func IncRobotConnectivityCritical(namespace string) {
	RobotConnectivityCriticalTotal.WithLabelValues(namespace).Inc()
}

// SetFleetAdapterState sets swarmada_fleet_adapter_connected (1 iff phase is
// Connected) and swarmada_fleet_adapter_phase (1 for the current phase, 0 for the
// rest) for one adapter (§9.3.8).
func SetFleetAdapterState(namespace, adapter, phase string, connected bool) {
	c := 0.0
	if connected {
		c = 1
	}
	FleetAdapterConnected.WithLabelValues(namespace, adapter).Set(c)
	for _, p := range AdapterPhases {
		v := 0.0
		if p == phase {
			v = 1
		}
		FleetAdapterPhase.WithLabelValues(namespace, adapter, p).Set(v)
	}
}
