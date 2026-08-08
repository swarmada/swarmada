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

package controlstream

import (
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// TelemetryFrame translates a wire TelemetryPayload (RFC-0001 §9.2.3) into the
// proto-free telemetry.Frame the projector consumes (§9.3.7). It is the
// integration seam documented in the telemetry package: the ControlStream server
// calls it once per received TelemetryPayload before invoking the Ingestor.
//
// Proto3 explicit presence on safety-relevant scalars is preserved end to end
// (RA-3b): an absent battery percent stays nil (distinct from a real 0% critical
// reading), and a pose is emitted only when a real 2-D fix is present (see
// [mapPosition]).
func TelemetryFrame(p *fav1.TelemetryPayload) telemetry.Frame {
	f := telemetry.Frame{
		RobotID:        p.GetRobotId(),
		Timestamp:      time.UnixMilli(p.GetTimestampMs()),
		Phase:          mapRobotPhase(p.GetPhase()),
		AssignedAction: p.GetCurrentAction(),
	}
	if b := p.GetBattery(); b != nil {
		// Read the raw pointer, NOT GetPercent(): the getter collapses an absent
		// reading to 0, erasing the presence distinction the wire preserves.
		f.BatteryPct = copyInt32(b.Percent)
	}
	if pos := mapPosition(p.GetPosition()); pos != nil {
		f.Position = pos
	}
	if hw := mapHardware(p.GetHardware()); len(hw) > 0 {
		f.Hardware = hw
	}
	return f
}

// mapPosition builds the TSDB-only pose. It honours the explicit-presence
// discipline (RA-3b) at the point that matters most — a phantom origin — by
// emitting a pose ONLY when both x and y are present; a dropped x or y therefore
// never fabricates (0,0). yaw and floor collapse to their zero value when absent,
// which is acceptable here because Position feeds only the time-series plane and
// never a Robot.status write (RA-1): it is never used for a material decision.
func mapPosition(p *fav1.RobotPosition) *telemetry.Position {
	if p == nil || p.X == nil || p.Y == nil {
		return nil
	}
	return &telemetry.Position{
		X:     *p.X,
		Y:     *p.Y,
		Yaw:   p.GetYaw(),
		Floor: p.GetFloor(),
	}
}

// mapHardware turns the delta list of component status updates into the map the
// projector merges onto its last-known full map. Entries with an empty component
// name or an UNSPECIFIED (unknown) status are skipped so the projector keeps the
// previously recorded value rather than clobbering it. Returns nil when nothing
// maps, so an all-unknown delta produces no hardware change.
func mapHardware(updates []*fav1.HardwareStatusUpdate) map[string]fleetv1.HardwareStatus {
	if len(updates) == 0 {
		return nil
	}
	out := make(map[string]fleetv1.HardwareStatus, len(updates))
	for _, u := range updates {
		name := u.GetComponentName()
		if name == "" {
			continue
		}
		status, ok := mapHardwareStatus(u.GetStatus())
		if !ok {
			continue
		}
		out[name] = status
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapHardwareStatus maps the proto HardwareStatus enum to the CRD one. The
// boolean is false for UNSPECIFIED (unknown), signalling the caller to leave the
// component's recorded status unchanged.
func mapHardwareStatus(s fav1.HardwareStatus) (fleetv1.HardwareStatus, bool) {
	switch s {
	case fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY:
		return fleetv1.HardwareHealthy, true
	case fav1.HardwareStatus_HARDWARE_STATUS_DEGRADED:
		return fleetv1.HardwareDegraded, true
	case fav1.HardwareStatus_HARDWARE_STATUS_FAILED:
		return fleetv1.HardwareFailed, true
	case fav1.HardwareStatus_HARDWARE_STATUS_DISABLED:
		return fleetv1.HardwareDisabled, true
	default:
		return "", false
	}
}

// mapRobotPhase maps the proto RobotPhase enum onto the CRD RobotPhase (§9.1.3
// enum set). UNSPECIFIED and any unknown value map to the empty phase, which the
// projector treats as "unreported" and leaves the recorded phase unchanged.
func mapRobotPhase(p fav1.RobotPhase) fleetv1.RobotPhase {
	switch p {
	case fav1.RobotPhase_ROBOT_PHASE_IDLE:
		return fleetv1.RobotPhaseIdle
	case fav1.RobotPhase_ROBOT_PHASE_ASSIGNED:
		return fleetv1.RobotPhaseAssigned
	case fav1.RobotPhase_ROBOT_PHASE_IN_PROGRESS:
		return fleetv1.RobotPhaseInProgress
	case fav1.RobotPhase_ROBOT_PHASE_CHARGING:
		return fleetv1.RobotPhaseCharging
	case fav1.RobotPhase_ROBOT_PHASE_ERROR:
		return fleetv1.RobotPhaseError
	case fav1.RobotPhase_ROBOT_PHASE_OFFLINE:
		return fleetv1.RobotPhaseOffline
	case fav1.RobotPhase_ROBOT_PHASE_MAINTENANCE:
		return fleetv1.RobotPhaseMaintenance
	case fav1.RobotPhase_ROBOT_PHASE_DISCOVERED:
		return fleetv1.RobotPhaseDiscovered
	default:
		return ""
	}
}

// copyInt32 clones an optional int32 so the Frame owns its own storage and does
// not alias the decoded proto message.
func copyInt32(v *int32) *int32 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
