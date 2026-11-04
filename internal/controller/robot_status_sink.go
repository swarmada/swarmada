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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// RobotIDAnnotation aliases the canonical API constant (api/v1) so controller code
// and its tests keep the package-local name. The annotation bridges the wire robot_id
// to the Robot CRD (RFC-0001 #control-plane-swarmada-api-server); it is single-sourced in api/v1 so the admission
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
// Field ownership. This sink projects the telemetry-owned health fields —
// BatteryPercent and Hardware — and one phase: Error, which the robot alone can
// report. A robot that reports Error is written Error, and assignment halts for it
// (RFC-0001 #safety-scope-and-responsibility-boundary, Layer 3: candidate filter 1
// admits only Idle robots); its next report that is not Error returns it to the phase a live robot
// settles into. No other reported phase is written. Phase and AssignedAction are
// otherwise owned by the scheduler and the robot lifecycle controllers and are
// single-executor-critical (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee),
// so the Error write never races them blind: it is made under the resourceVersion the
// sink read. So is a Hardware write, because the RobotProbe controller writes that list
// too and a merge patch replaces all of it: from a view older than the probe's last write
// it put back what the view held, and erased the probe's verdict. A conflict reads the
// robot again, uncached, and redoes the write on what it reads. A robot in
// a liveness-owned phase (Discovered, Offline) is left to the Robot reconciler, which
// reads the same report when it advances the robot. Connectivity transitions belong to
// the Robot reconciler and the FleetAdapter controller (RFC-0001 #control-plane-robot-health-and-connectivity);
// robot-reported action state is reconciled by the reconnect protocol (RFC-0001
// #fleet-adapter-protocol-reconnect-action-state-reconciliation). Both are separate
// from this projection.
type RobotStatusSink struct {
	client.Client
	// APIReader reads the robot uncached, for the fresh read after a write lost to a
	// conflict. Nil falls back to Client.
	APIReader client.Reader
}

// ApplyMaterialUpdate writes the changed telemetry-owned fields of upd onto the
// robot's status, and the robot-reported Error phase (see RobotStatusSink). A
// material transition that changes nothing this sink writes (a reported phase
// other than Error on a robot not in Error, for instance) produces no status write,
// and wrote is false.
func (s *RobotStatusSink) ApplyMaterialUpdate(ctx context.Context, upd telemetry.MaterialUpdate) (wrote bool, err error) {
	if upd.BatteryPct == nil && upd.Hardware == nil && upd.Phase == nil {
		// Nothing this sink writes changed.
		return false, nil
	}

	robot, err := findTelemetryRobot(ctx, s.Client, upd.Namespace, upd.RobotID)
	if err != nil {
		return false, err
	}
	if robot == nil {
		// robot_id is not yet mapped to a Robot (not admitted, or the
		// swarmada.io/robot-id annotation is absent). Drop rather than fail:
		// telemetry for an unknown robot must not wedge the ingestion path.
		return false, nil
	}

	reread, gone := false, false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if reread {
			key := client.ObjectKeyFromObject(robot)
			*robot = fleetv1.Robot{}
			if err := s.robotReader().Get(ctx, key, robot); err != nil {
				// Deleted since the first read: there is nothing left to write.
				gone = apierrors.IsNotFound(err)
				return client.IgnoreNotFound(err)
			}
		}
		reread = true
		wrote = false
		original := robot.DeepCopy()
		if upd.BatteryPct != nil {
			pct := *upd.BatteryPct
			robot.Status.BatteryPercent = &pct
		}
		if upd.Hardware != nil {
			held, err := s.probeHeldComponents(ctx, robot, upd.Hardware)
			if err != nil {
				return err
			}
			robot.Status.Hardware = applyHardware(robot.Status.Hardware, upd.Hardware, held)
		}
		if upd.Phase != nil {
			if phase, ok := reportedPhaseTransition(robot, *upd.Phase); ok {
				robot.Status.Phase = phase
			}
		}
		if equality.Semantic.DeepEqual(original.Status, robot.Status) {
			// The stored status already holds every value (the first frame after a
			// restart, or a reported phase this sink does not write): no write.
			return nil
		}
		patch := client.MergeFrom(original)
		if robot.Status.Phase != original.Status.Phase ||
			!equality.Semantic.DeepEqual(robot.Status.Hardware, original.Status.Hardware) {
			patch = client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})
		}
		if err := s.Status().Patch(ctx, robot, patch); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if err != nil || gone {
		return false, err
	}
	return wrote, nil
}

// robotReader is the uncached APIReader when one is wired, else Client.
func (s *RobotStatusSink) robotReader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

// reportedPhaseTransition is the phase a robot's report moves it to, if any: Error
// when it reports Error, and the phase a live robot settles into when a robot in
// Error reports anything else. A liveness-owned phase is the Robot reconciler's to
// advance (livePhase), so it is not moved here.
func reportedPhaseTransition(robot *fleetv1.Robot, reported fleetv1.RobotPhase) (fleetv1.RobotPhase, bool) {
	current := robot.Status.Phase
	switch {
	case isLivenessOwnedPhase(current):
		return "", false
	case reported == fleetv1.RobotPhaseError && current != fleetv1.RobotPhaseError:
		return fleetv1.RobotPhaseError, true
	case reported != fleetv1.RobotPhaseError && current == fleetv1.RobotPhaseError:
		return steadyPhaseForLive(robot), true
	}
	return "", false
}

// findTelemetryRobot resolves a telemetry frame's wire robot_id to its Robot. It is
// shared by both telemetry consumers, this sink and ZoneController.ObservePosition.
//
// The robot-id annotation is unique per namespace, not across namespaces (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3): two namespaces may each hold a robot with the same id. So
// when the stream declared a namespace, the lookup is scoped to it
// (findRobotByIDInNamespace), and two robots sharing an id in different namespaces
// each resolve to their own object. Inside one namespace a duplicate is still a
// violation, and that lookup still refuses it.
//
// A frame can arrive with no namespace. An adapter with a verified client certificate
// is always served in the certificate's namespace, whether or not its AdapterHello
// declared one (controlstream.bindHelloNamespace); a stream with no verified
// certificate is served in whatever its hello declared, which may be nothing. Then
// there is nothing to scope to, and the lookup falls back to a cluster-wide search.
// One match resolves. More than one is refused, because nothing on the frame says
// which robot it describes, and the error names that reason rather than calling the
// id duplicated. The List is served from the
// controller cache; material writes and zone transitions are rare, so no field indexer
// backs it yet.
func findTelemetryRobot(ctx context.Context, c client.Client, namespace, robotID string) (*fleetv1.Robot, error) {
	if robotID == "" {
		return nil, nil
	}
	if namespace != "" {
		return findRobotByIDInNamespace(ctx, c, namespace, robotID)
	}
	var list fleetv1.RobotList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing robots to resolve robot_id %q: %w", robotID, err)
	}
	var match *fleetv1.Robot
	for i := range list.Items {
		if list.Items[i].Annotations[RobotIDAnnotation] != robotID {
			continue
		}
		if match != nil && match.Namespace == list.Items[i].Namespace {
			// A duplicate inside one namespace is a violation whatever the stream declared.
			return nil, fmt.Errorf("robot_id %q maps to more than one Robot in namespace %q",
				robotID, match.Namespace)
		}
		if match != nil {
			return nil, fmt.Errorf(
				"robot_id %q maps to more than one Robot, in namespaces %q and %q, and the adapter "+
					"stream declared no namespace, so the robot cannot be identified; set namespace "+
					"in the adapter's AdapterHello",
				robotID, match.Namespace, list.Items[i].Namespace)
		}
		match = &list.Items[i]
	}
	return match, nil
}

// CO-OWNERSHIP (RFC-0001 #robotprobe-probe-failure-effect-what-happens-downstream): Robot.status.hardware[] is ALSO written by the RobotProbe controller,
// which degrades a component after failureThreshold consecutive probe failures, and restores it
// only after recoveryThreshold consecutive passes. The adapter's own view must not undo that
// verdict: a component the adapter believes healthy while a probe says otherwise is exactly the
// case a probe exists to catch. So a PROBE-HELD component (see probeHeldComponents) stays at least
// Degraded whatever telemetry reports, and only the probe that set it clears it. Telemetry may
// still make it worse (Failed).
//
// The verdict is read from the API, not from memory, so it holds across a manager restart: the
// projector's first frame after a restart carries the adapter's full map, and this is where that
// map meets the probe's verdict.
//
// probeHeldComponents returns the components of robot whose current Degraded or Failed status is a
// probe verdict this telemetry update would lower. A component is probe-held when its entry carries
// a degradationReason (written only by the RobotProbe controller, which always sets one when it
// degrades a component) AND a hardware RobotProbe in the namespace still targets that component
// and still reports on this robot. The second half is what releases a verdict whose probe was
// deleted or no longer selects the robot: nothing else would ever clear it. The RobotProbe list is
// read only when some component is a candidate, which is rare; an ordinary telemetry write lists
// nothing.
func (s *RobotStatusSink) probeHeldComponents(
	ctx context.Context,
	robot *fleetv1.Robot,
	updates map[string]fleetv1.HardwareStatus,
) (map[string]bool, error) {
	var candidates []string
	for _, h := range robot.Status.Hardware {
		st, ok := updates[h.Name]
		if !ok || h.DegradationReason == "" || !probeVerdictStatus(h.Status) {
			continue
		}
		if severity(st) < severity(h.Status) {
			candidates = append(candidates, h.Name)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	var probes fleetv1.RobotProbeList
	if err := s.List(ctx, &probes, client.InNamespace(robot.Namespace)); err != nil {
		return nil, fmt.Errorf("listing robot probes for held hardware verdicts: %w", err)
	}
	held := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		for i := range probes.Items {
			if probeReportsOn(&probes.Items[i], name, robot.Name) {
				held[name] = true
				break
			}
		}
	}
	return held, nil
}

// probeReportsOn reports whether rp is a hardware probe of component that still carries a
// result for robot.
func probeReportsOn(rp *fleetv1.RobotProbe, component, robot string) bool {
	if rp.Spec.ProbeType != fleetv1.ProbeTypeHardware || rp.Spec.TargetComponent != component {
		return false
	}
	for _, res := range rp.Status.RobotResults {
		if res.RobotName == robot {
			return true
		}
	}
	return false
}

// probeVerdictStatus is the set of statuses a probe verdict can hold: the probe writes
// Degraded, and telemetry may have worsened it to Failed since.
func probeVerdictStatus(s fleetv1.HardwareStatus) bool {
	return s == fleetv1.HardwareDegraded || s == fleetv1.HardwareFailed
}

// severity orders hardware statuses for the held-verdict rule. Disabled is an operator state,
// not a health reading, and ranks with Healthy.
func severity(s fleetv1.HardwareStatus) int {
	switch s {
	case fleetv1.HardwareFailed:
		return 2
	case fleetv1.HardwareDegraded:
		return 1
	}
	return 0
}

// applyHardware folds the projector's full component→status map onto the robot's
// existing status list: it updates the Status of matching components (preserving
// LastHealthyAt / DegradationReason / DegradedMetrics) and appends entries for
// newly-seen components in deterministic (sorted) order. A component in held keeps
// at least Degraded. It never removes a component. It does not mutate the input
// slice.
func applyHardware(
	existing []fleetv1.HardwareComponentStatus,
	updates map[string]fleetv1.HardwareStatus,
	held map[string]bool,
) []fleetv1.HardwareComponentStatus {
	out := make([]fleetv1.HardwareComponentStatus, len(existing))
	copy(out, existing)

	seen := make(map[string]bool, len(out))
	for i := range out {
		if st, ok := updates[out[i].Name]; ok {
			if held[out[i].Name] && severity(st) < severity(fleetv1.HardwareDegraded) {
				st = fleetv1.HardwareDegraded
			}
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
