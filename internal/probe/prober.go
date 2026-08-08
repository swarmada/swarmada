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

// Package probe runs active RobotProbe health checks (RFC-0001 §9.1.6): it pushes
// the verify_hardware / verify_capability / verify_model Commands to a robot's
// Fleet Adapter and binds the returned proto ProbeStatus into a control-plane
// status. Binding is fail-safe: a probe that cannot confirm health is never
// reported Healthy.
package probe

import (
	"context"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Status is the control-plane result of a probe cycle for one robot.
type Status string

// Status values. Unknown is distinct from Failed: Unknown means the adapter
// does not implement probes (§9.1.6.3), Failed means the check ran and the
// component is unreachable or out of spec.
const (
	StatusHealthy  Status = "Healthy"
	StatusDegraded Status = "Degraded"
	StatusFailed   Status = "Failed"
	StatusUnknown  Status = "Unknown"
)

// Result is the control-plane outcome of one verify RPC.
type Result struct {
	// Status is the bound proto ProbeStatus.
	Status Status
	// ActualMetrics are the values the adapter measured.
	ActualMetrics map[string]float64
	// Message is the adapter's human-readable summary.
	Message string
	// Unsupported is true when the adapter declined the probe
	// (CommandResult.unsupported) — all probes to it report Unknown permanently.
	Unsupported bool
}

// VerifyRequest is the input to one verify_* probe RPC. Target is the
// component/capability/model name resolved from ProbeType by the caller;
// SyntheticInput is a model probe's test input (VerifyModel.synthetic_input) and is
// nil for hardware/capability probes. Expected are the metric thresholds the result
// is checked against.
type VerifyRequest struct {
	ProbeType      fleetv1.ProbeType
	Target         string
	Expected       map[string]float64
	SyntheticInput []byte
}

// Prober invokes a verify_* Command on a robot's Fleet Adapter and returns the
// result. The production implementation pushes the Command over the ControlStream
// command-push path (§9.2, backlog §E-2) and binds the CommandResult.VerifyResult;
// it MUST be safe for concurrent use. A non-nil error means the probe could not be
// completed (unreachable/timeout) — the caller treats that as Failed, never
// Healthy.
type Prober interface {
	Verify(ctx context.Context, namespace, robotID string, req VerifyRequest) (Result, error)
}

// BindProbeStatus maps the proto ProbeStatus enum to the control-plane status
// (§9.1.6.3). UNSPECIFIED — a status the adapter never legitimately reports — binds
// to Unknown, never to a healthy state.
func BindProbeStatus(s fav1.ProbeStatus) Status {
	switch s {
	case fav1.ProbeStatus_PROBE_STATUS_HEALTHY:
		return StatusHealthy
	case fav1.ProbeStatus_PROBE_STATUS_DEGRADED:
		return StatusDegraded
	case fav1.ProbeStatus_PROBE_STATUS_FAILED:
		return StatusFailed
	default:
		return StatusUnknown
	}
}

// MetricsMet reports whether every expected threshold is satisfied by the actual
// metrics. A missing actual metric fails the check (never assumed met). Thresholds
// are minimums (the §9.1.6.3 metrics are "minimum acceptable value").
func MetricsMet(expected, actual map[string]float64) bool {
	for name, threshold := range expected {
		got, ok := actual[name]
		if !ok || got < threshold {
			return false
		}
	}
	return true
}
