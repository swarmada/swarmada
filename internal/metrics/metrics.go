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

// Package metrics registers and emits the RFC-0001 §9.3.8 control-plane
// Prometheus metrics. It is ADDITIVE instrumentation only — no reconciler
// behaviour depends on it (RA-1 is untouched: emitting a metric never writes
// resource status).
//
// Names, types, labels, and histogram buckets are taken verbatim from §9.3.8.
// Per §9.3.8 "Metric registration requirement", every Gauge label combination
// known at startup is initialised to 0 (see InitNamespace / InitAdapter) so a
// missing label set cannot be misread as a zero-count state; Counters are simply
// registered and never reset.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Enum label value sets from §9.3.8 — the fixed dimensions Gauges must be
// initialised across for a namespace/adapter (the dynamic namespace/adapter
// dimensions are seeded lazily via InitNamespace / InitAdapter).
var (
	// FleetActionPhases are the swarmada_fleetactions_by_phase `phase` values (§9.3.8).
	FleetActionPhases = []string{
		"Pending", "Assigned", "InProgress", "Revoking", "Paused", "Preempted", "Succeeded", "Failed", "Cancelled",
	}
	// RobotPhases are the swarmada_robots_by_phase `phase` values (§9.3.8).
	RobotPhases = []string{
		"Discovered", "Idle", "Assigned", "InProgress", "Charging", "Error", "Offline", "Maintenance",
	}
	// AdapterPhases are the swarmada_fleet_adapter_phase `phase` values (§9.3.8).
	AdapterPhases = []string{"Pending", "Connected", "Degraded", "Disconnected", "Rejected"}
	// EstopStates are the swarmada_robots_in_estop `estop_state` values (§9.3.8).
	// Failed is included: a robot whose estop could not be CONFIRMED is withheld from
	// dispatch, which is precisely the robot an operator is debugging. Omitting it made
	// that robot invisible to the gauge while the sweeper was already counting it.
	EstopStates = []string{"Stopping", "Stopped", "Failed"}
)

// ── Estop metrics (§9.3.8) ────────────────────────────────────────────────────

var (
	// EstopCommandLatencySeconds — SafetyStream estop send → EstopAck receipt.
	EstopCommandLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "swarmada_estop_command_latency_seconds",
		Help:    "Round-trip latency from estop Command send on SafetyStream to EstopAck receipt (RFC-0001 §9.3.8). The 0.5 bucket is the SLA boundary.",
		Buckets: []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1.0, 2.0, 5.0},
	}, []string{"namespace", "adapter", "robot_name", "scope"})

	// EstopLatencyViolationsTotal — estop ACK latency exceeded the 500ms SLA.
	EstopLatencyViolationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_estop_latency_violations_total",
		Help: "estop Command ACK latency exceeding the 500ms SLA (RFC-0001 §9.3.8). Any non-zero rate is an SLO breach.",
	}, []string{"namespace", "adapter", "robot_name"})

	// EstopFanoutDurationSeconds — a whole zone-/namespace-scoped estop episode, from the
	// operator's trigger to the last robot in scope resolving (ADR-0042).
	//
	// This exists because swarmada_estop_command_latency_seconds structurally cannot see
	// the sequential-fan-out gap: it is stamped per robot just before THAT robot's send,
	// so sequential dispatch delays the send rather than the round trip and every robot
	// reports healthy. Buckets run past the per-send 500ms SLA into tens of seconds
	// because that is the range a sequential fan-out over a large fleet actually occupies.
	EstopFanoutDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "swarmada_estop_fanout_duration_seconds",
		Help:    "Wall-clock duration of one zone- or namespace-scoped estop episode, from trigger to the last robot in scope resolving (RFC-0001 §9.3.8, §9.6.2.2). Robot-scoped estops have no fan-out and are not observed here.",
		Buckets: []float64{0.5, 1.0, 2.0, 5.0, 10.0, 30.0, 60.0, 120.0, 300.0},
	}, []string{"namespace", "scope"})

	// EstopCommandsTotal — every estop Command by terminal disposition.
	EstopCommandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_estop_commands_total",
		Help: "Total estop Commands issued (RFC-0001 §9.3.8). result: ack_stopping, ack_stopped, ack_failed, timeout. scope: robot, zone, namespace.",
	}, []string{"namespace", "adapter", "scope", "result"})
)

// ── Telemetry pipeline metrics (§9.3.8) ───────────────────────────────────────

var (
	// TelemetryDroppedFramesTotal — frames not forwardable to the TSDB sink.
	TelemetryDroppedFramesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_telemetry_dropped_frames_total",
		Help: "Telemetry frames not forwardable to the configured TSDB sink (RFC-0001 §9.3.8 / telemetry Invariant 1). Never increments when sink.type=Drop.",
	}, []string{"namespace", "adapter"})

	// TelemetryTSDBWriteErrorsTotal — errors from the TSDBWriter interface.
	TelemetryTSDBWriteErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_telemetry_tsdb_write_errors_total",
		Help: "Errors returned by the TSDBWriter on write attempts (RFC-0001 §9.3.8). error_class: timeout, auth, schema, unknown.",
	}, []string{"namespace", "sink_type", "error_class"})

	// TelemetryStatusWritesTotal — Robot.status writes on material transitions.
	TelemetryStatusWritesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_telemetry_status_writes_total",
		Help: "Robot.status writes by the StatusProjector on material transitions (RFC-0001 §9.3.8). transition_type: phase_change, hardware_health_change, battery_threshold, assigned_task_change, safety_critical.",
	}, []string{"namespace", "transition_type"})

	// TelemetryFramesReceivedTotal — TelemetryPayload frames received on ControlStream.
	TelemetryFramesReceivedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_telemetry_frames_received_total",
		Help: "Total TelemetryPayload frames received on ControlStream (RFC-0001 §9.3.8). The status_writes/frames_received ratio quantifies write-amplification suppression.",
	}, []string{"namespace", "adapter"})
)

// ── Scheduler & assignment-lease metrics (§9.3.8) ─────────────────────────────

var (
	// SchedulerAssignmentLatencySeconds — Pending (or re-Pending from Revoking) → Assigned.
	SchedulerAssignmentLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "swarmada_scheduler_assignment_latency_seconds",
		Help:    "Time from a FleetAction entering Pending (or re-entering from Revoking) to the Assigned transition (RFC-0001 §9.3.8).",
		Buckets: []float64{0.1, 0.5, 1.0, 5.0, 15.0, 30.0, 60.0, 120.0, 300.0},
	}, []string{"namespace", "priority"})

	// SchedulerAssignmentFailuresTotal — assignments ending Failed or abandoned.
	SchedulerAssignmentFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_scheduler_assignment_failures_total",
		Help: "Assignments ending in Failed or abandoned without assignment (RFC-0001 §9.3.8). reason: DeadlineExceeded, NoCandidates, AdapterDisconnected, AllCandidatesInEstop, AllCandidatesInMaintenance.",
	}, []string{"namespace", "reason"})

	// SchedulerLeaseRenewalsTotal — successful renew_lease Commands.
	SchedulerLeaseRenewalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_scheduler_lease_renewals_total",
		Help: "Successful renew_lease Commands issued to Fleet Adapters (RFC-0001 §9.3.8).",
	}, []string{"namespace"})

	// SchedulerLeaseExpiriesTotal — leases that expired without prior confirmed cancel.
	SchedulerLeaseExpiriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_scheduler_lease_expiries_total",
		Help: "Leases that expired (now >= last-ack'd renewal + leaseDurationSeconds + clockSkewMarginSeconds) without a prior confirmed cancellation (RFC-0001 §9.3.8).",
	}, []string{"namespace"})
)

// ── FleetAction phase gauge (§9.3.8) ────────────────────────────────────────────

// FleetActionsByPhase — current FleetAction count per status.phase.
var FleetActionsByPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "swarmada_fleetactions_by_phase",
	Help: "Current count of FleetAction resources per status.phase (RFC-0001 §9.3.8). The Revoking value is initialised to 0 at startup.",
}, []string{"namespace", "phase"})

// ── FleetAdapter connectivity metrics (§9.3.8) ────────────────────────────────

var (
	// FleetAdapterConnected — 1 if the adapter has an active ControlStream, else 0.
	FleetAdapterConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmada_fleet_adapter_connected",
		Help: "1 if the FleetAdapter has an active ControlStream to the API Server, 0 otherwise (RFC-0001 §9.3.8). 0 means no Command — including estop — can reach that adapter's robots.",
	}, []string{"namespace", "adapter"})

	// FleetAdapterPhase — 1 for the adapter's current status.phase, 0 for others.
	FleetAdapterPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmada_fleet_adapter_phase",
		Help: "1 for the current FleetAdapter.status.phase, 0 for all others (RFC-0001 §9.3.8). phase: Pending, Connected, Degraded, Disconnected, Rejected.",
	}, []string{"namespace", "adapter", "phase"})

	// FleetAdapterReconnectsTotal — successful ControlStream re-establishments.
	FleetAdapterReconnectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_fleet_adapter_reconnects_total",
		Help: "Times a FleetAdapter successfully re-established its ControlStream after Disconnected or Degraded (RFC-0001 §9.3.8).",
	}, []string{"namespace", "adapter"})
)

// ── Robot phase & estop-state gauges (§9.3.8) ─────────────────────────────────

var (
	// RobotsByPhase — current Robot count per status.phase.
	RobotsByPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmada_robots_by_phase",
		Help: "Current count of Robot resources per status.phase (RFC-0001 §9.3.8).",
	}, []string{"namespace", "phase"})

	// RobotsInEstop — current robot count per active estop state (Stopping/Stopped).
	RobotsInEstop = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "swarmada_robots_in_estop",
		Help: "Current count of robots per active estop state (RFC-0001 §9.3.8). estop_state: Stopping, Stopped (Normal excluded).",
	}, []string{"namespace", "estop_state"})

	// RobotOfflineDurationSeconds — time a robot stayed Offline before reconnecting.
	RobotOfflineDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "swarmada_robot_offline_duration_seconds",
		Help:    "Duration a robot remained Offline before reconnecting, observed at reconnect (RFC-0001 §9.3.8).",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"namespace"})

	// RobotConnectivityCriticalTotal — escalations to the ConnectivityCritical
	// condition (a robot Offline beyond spec.health.connectivityCriticalThresholdSeconds).
	// Counted on the False→True edge only (ADR-0011).
	RobotConnectivityCriticalTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "swarmada_robot_connectivity_critical_total",
		Help: "Escalations to ConnectivityCritical (robot Offline beyond the critical threshold), counted on the escalation edge (ADR-0011).",
	}, []string{"namespace"})
)

// ── Build info (§9.3.8) ───────────────────────────────────────────────────────

// BuildVersion is the control-plane version reported by swarmada_version. It is
// overridden at link time (-ldflags "-X .../internal/metrics.BuildVersion=v0.1.0");
// the default names the state honestly rather than asserting a version that was
// never stamped.
var BuildVersion = "unknown"

// SwarmadaVersion is the §9.3.8 build-info gauge: a constant 1 carrying the
// control-plane version as a label. The value is meaningless on its own — the
// point is the label, which lets an operator join any other series against the
// build that produced it, and lets an upgrade be seen as a change in the label set
// rather than inferred from a restart.
var SwarmadaVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "swarmada_version",
	Help: "Control-plane build info (RFC-0001 §9.3.8). Always 1; the version label carries the value.",
}, []string{"version"})

// collectors is every §9.3.8 metric — the exhaustive set Register installs.
func collectors() []prometheus.Collector {
	return []prometheus.Collector{
		EstopCommandLatencySeconds,
		EstopFanoutDurationSeconds,
		EstopLatencyViolationsTotal,
		EstopCommandsTotal,
		TelemetryDroppedFramesTotal,
		TelemetryTSDBWriteErrorsTotal,
		TelemetryStatusWritesTotal,
		TelemetryFramesReceivedTotal,
		SchedulerAssignmentLatencySeconds,
		SchedulerAssignmentFailuresTotal,
		SchedulerLeaseRenewalsTotal,
		SchedulerLeaseExpiriesTotal,
		FleetActionsByPhase,
		FleetAdapterConnected,
		FleetAdapterPhase,
		FleetAdapterReconnectsTotal,
		RobotsByPhase,
		RobotsInEstop,
		RobotOfflineDurationSeconds,
		RobotConnectivityCriticalTotal,
		SwarmadaVersion,
	}
}

// Register installs every §9.3.8 collector into r (cmd/manager passes the
// controller-runtime registry so the manager's /metrics endpoint, default :8080,
// serves them). It panics on a duplicate registration — call it exactly once per
// registry at startup, which is the §9.3.8 "MustRegister at startup" contract.
func Register(r prometheus.Registerer) {
	r.MustRegister(collectors()...)
	// A build-info gauge that is registered but never set exports nothing, which is
	// indistinguishable from the metric not existing at all — so set it here rather
	// than leaving it to a caller.
	SwarmadaVersion.WithLabelValues(BuildVersion).Set(1)
}

// InitNamespace seeds the namespace-scoped enum gauges to 0 for ns, so a phase or
// estop-state that has not yet been observed reads as 0 rather than absent
// (§9.3.8 registration requirement). Call it when a namespace is first reconciled.
// The adapter-keyed gauges are seeded separately by InitAdapter (adapter names are
// not known until an adapter appears).
func InitNamespace(ns string) {
	for _, p := range FleetActionPhases {
		FleetActionsByPhase.WithLabelValues(ns, p).Add(0)
	}
	for _, p := range RobotPhases {
		RobotsByPhase.WithLabelValues(ns, p).Add(0)
	}
	for _, s := range EstopStates {
		RobotsInEstop.WithLabelValues(ns, s).Add(0)
	}
}

// InitAdapter seeds the per-adapter enum gauges to 0 (connectivity + phase), so a
// newly-discovered adapter reports connected=0 and a full phase vector before its
// first status computation (§9.3.8).
func InitAdapter(ns, adapter string) {
	FleetAdapterConnected.WithLabelValues(ns, adapter).Add(0)
	for _, p := range AdapterPhases {
		FleetAdapterPhase.WithLabelValues(ns, adapter, p).Add(0)
	}
}
