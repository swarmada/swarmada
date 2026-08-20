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
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// RobotIDAnnotation aliases the canonical API constant (api/v1) so controller code
// and its tests keep the package-local name. The annotation bridges the wire robot_id
// to the Robot CRD (RFC-0001 §9.3.1); it is single-sourced in api/v1 so the admission
// webhook can reference it without importing this package.
const RobotIDAnnotation = fleetv1.RobotIDAnnotation

// RobotStatusSink implements telemetry.StatusSink: it writes the material
// telemetry projection to Robot.status.
//
// RA-1 discipline. ApplyMaterialUpdate is invoked by the telemetry Ingestor ONLY
// on a material transition the Projector has already approved — never on a
// per-tick frame. The projector coalesces unchanged frames to zero writes and
// caps write frequency, so this sink cannot become a per-tick etcd sink.
//
// Field ownership. This sink projects only the telemetry-owned health fields —
// BatteryPercent and Hardware. It deliberately
// does NOT write Phase or AssignedAction: those are owned by the scheduler and the
// robot lifecycle controllers and are single-executor-critical (§9.6.3.5).
// Racing them from the telemetry plane would be unsafe. Telemetry-driven phase
// and connectivity transitions belong to the Robot reconciler and the FleetAdapter
// controller (§9.3.3); robot-reported action state is reconciled by the reconnect
// protocol (§9.2.6). All are separate from this projection.
type RobotStatusSink struct {
	client.Client
}

// ApplyMaterialUpdate writes the changed telemetry-owned fields of upd onto the
// robot's status. A material transition that changes only a non-projected field
// (e.g. a phase-only change) produces no status write here.
func (s *RobotStatusSink) ApplyMaterialUpdate(ctx context.Context, upd telemetry.MaterialUpdate) error {
	if upd.BatteryPct == nil && upd.Hardware == nil {
		// Nothing telemetry-owned changed (e.g. a phase-only material transition).
		return nil
	}

	robot, err := s.resolveRobot(ctx, upd.RobotID)
	if err != nil {
		return err
	}
	if robot == nil {
		// robot_id is not yet mapped to a Robot (not admitted, or the
		// swarmada.io/robot-id annotation is absent). Drop rather than fail:
		// telemetry for an unknown robot must not wedge the ingestion path.
		return nil
	}

	original := robot.DeepCopy()
	if upd.BatteryPct != nil {
		pct := *upd.BatteryPct
		robot.Status.BatteryPercent = &pct
	}
	if upd.Hardware != nil {
		robot.Status.Hardware = applyHardware(robot.Status.Hardware, upd.Hardware)
	}
	return s.Status().Patch(ctx, robot, client.MergeFrom(original))
}

// resolveRobot finds the Robot carrying RobotIDAnnotation == robotID. robot_id is
// globally unique (§9.2.3), so the lookup is cluster-wide and expects exactly one
// match: zero → nil (unmapped), more than one → an error (a spec violation; the
// sink refuses to guess which robot to write). The List is served from the
// controller cache. TODO: back this with a field indexer if material-write volume
// grows; material writes are rare today (projector-gated).
func (s *RobotStatusSink) resolveRobot(ctx context.Context, robotID string) (*fleetv1.Robot, error) {
	if robotID == "" {
		return nil, nil
	}
	var list fleetv1.RobotList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing robots to resolve robot_id %q: %w", robotID, err)
	}
	var match *fleetv1.Robot
	for i := range list.Items {
		if list.Items[i].Annotations[RobotIDAnnotation] == robotID {
			if match != nil {
				return nil, fmt.Errorf(
					"robot_id %q maps to more than one Robot via annotation %s; refusing to project telemetry",
					robotID, RobotIDAnnotation)
			}
			match = &list.Items[i]
		}
	}
	return match, nil
}

// CO-OWNERSHIP (§9.1.6.5): Robot.status.hardware[] is ALSO written by the RobotProbe controller,
// which degrades a component after failureThreshold consecutive probe failures. RFC-0001 intends
// both writers ("a probe-reported health change is indistinguishable from a telemetry-reported
// one"), but the fold below is last-writer-wins over the adapter's FULL map: the next telemetry
// hardware update will clear a probe-set Degraded if the adapter still believes the component is
// healthy — which is exactly the case a probe exists to catch. Reconciling that (severity merge, or
// refusing to upgrade a probe-degraded component until the probe recovers) is an open design point,
// recorded here rather than left as a surprise.
//
// applyHardware folds the projector's full component→status map onto the robot's
// existing status list: it updates the Status of matching components (preserving
// LastHealthyAt / DegradationReason / DegradedMetrics) and appends entries for
// newly-seen components in deterministic (sorted) order. It never removes a
// component. It does not mutate the input slice.
func applyHardware(
	existing []fleetv1.HardwareComponentStatus,
	updates map[string]fleetv1.HardwareStatus,
) []fleetv1.HardwareComponentStatus {
	out := make([]fleetv1.HardwareComponentStatus, len(existing))
	copy(out, existing)

	seen := make(map[string]bool, len(out))
	for i := range out {
		if st, ok := updates[out[i].Name]; ok {
			out[i].Status = st
		}
		seen[out[i].Name] = true
	}

	added := make([]string, 0, len(updates))
	for name := range updates {
		if !seen[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		out = append(out, fleetv1.HardwareComponentStatus{Name: name, Status: updates[name]})
	}
	return out
}
