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
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// CapabilitiesIngestor projects an adapter's advertised supported-action catalog
// (carried in a CapabilitiesSnapshot, §9.2) onto FleetAdapter.status.supportedActions.
// The snapshot is per-robot; the catalog it carries is adapter-level, so the robot
// is resolved to its managing FleetAdapter and the catalog written there. RA-1: the
// status write happens only when the catalog changed, never on a telemetry tick.
type CapabilitiesIngestor struct {
	client.Client

	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
}

// IngestCapabilities projects snap.supported_actions onto the managing
// FleetAdapter's status. It is a no-op (nil) when the robot or adapter is absent
// or the catalog is unchanged. Errors are returned only for unexpected API failures.
func (i *CapabilitiesIngestor) IngestCapabilities(ctx context.Context, namespace string, snap *fav1.CapabilitiesSnapshot) error {
	if snap == nil || snap.GetRobotId() == "" {
		return nil
	}
	// Resolved by the robot-id ANNOTATION, not by treating robot_id as the object name.
	// They differ whenever an operator admitted with --name, and a name lookup silently
	// finds nothing for exactly those robots (ADR-0028).
	robot, err := findRobotByIDInNamespace(ctx, i.Client, namespace, snap.GetRobotId())
	if err != nil {
		return err
	}
	if robot == nil {
		return nil
	}

	// Adapter-reported install state (ADR-0033). Projected before the catalog so a snapshot
	// that names no adapter still lands the robot's own report — the recovery path for an
	// outcome whose stream was lost must not depend on the catalog write succeeding.
	if err := i.projectInstallState(ctx, robot, snap); err != nil {
		return err
	}

	// SensorStream discovery. Per-robot, like hardware and
	// installed models above — unlike supported_actions below, which is adapter-wide and
	// lands on FleetAdapter.status — so it is projected onto THIS robot, not the adapter.
	if err := i.projectSensorStreams(ctx, robot, snap); err != nil {
		return err
	}

	// Operating-subject declaration (FA-R1). Per-robot, exactly like sensor streams
	// above — the amendment names that path explicitly ("the same path sensor_streams
	// already takes"), so this is the same call in the same place rather than a second
	// projection path for the same kind of data.
	if err := i.projectHonouredOperatingConstraints(ctx, robot, snap); err != nil {
		return err
	}

	adapterName := robot.Spec.Adapter.Name
	if adapterName == "" {
		return nil
	}
	adapter := &fleetv1.FleetAdapter{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: namespace, Name: adapterName}, adapter); err != nil {
		return client.IgnoreNotFound(err)
	}
	desired := mapSupportedActions(snap.GetSupportedActions())
	if reflect.DeepEqual(adapter.Status.SupportedActions, desired) {
		return nil // RA-1: catalog unchanged, no status write.
	}
	// Patch the status subresource under retry-on-conflict: a concurrent FleetAdapter
	// reconcile ("object has been modified") is retried, not surfaced as an error, and
	// only the changed field is sent (ADR-0026).
	key := client.ObjectKeyFromObject(adapter)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &fleetv1.FleetAdapter{}
		if err := i.Get(ctx, key, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if reflect.DeepEqual(fresh.Status.SupportedActions, desired) {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.SupportedActions = desired
		return i.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// projectInstallState writes the robot's reported firmware and model state onto its own
// status. This is the RECOVERY path: a terminal UpdateProgress is lost if the control plane
// restarted or the stream dropped mid-install, and without this the rollout would wait
// forever for a message that already came and went.
func (i *CapabilitiesIngestor) projectInstallState(ctx context.Context, robot *fleetv1.Robot,
	snap *fav1.CapabilitiesSnapshot,
) error {
	key := client.ObjectKeyFromObject(robot)
	if err := projectFirmwareState(ctx, i.Client, key,
		firmwareStateFromSnapshot(snap.GetFirmware(), i.clock())); err != nil {
		return err
	}
	for _, m := range snap.GetInstalledModels() {
		entry, ok := modelEntryFromSnapshot(m)
		if !ok {
			continue
		}
		if err := projectModelState(ctx, i.Client, key, entry); err != nil {
			return err
		}
	}
	return nil
}

// projectSensorStreams writes snap.sensor_streams onto robot.status.sensorStreams,
// exactly as supported_actions is projected below — RA-1: only on change, under
// retry-on-conflict. Unlike supported_actions (adapter-wide), a sensor stream is a
// property of THIS robot, so it lands directly on the Robot, with no FleetAdapter
// lookup needed.
func (i *CapabilitiesIngestor) projectSensorStreams(ctx context.Context, robot *fleetv1.Robot, snap *fav1.CapabilitiesSnapshot) error {
	desired := mapSensorStreams(snap.GetSensorStreams())
	if reflect.DeepEqual(robot.Status.SensorStreams, desired) {
		return nil // RA-1: unchanged, no status write.
	}
	key := client.ObjectKeyFromObject(robot)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &fleetv1.Robot{}
		if err := i.Get(ctx, key, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if reflect.DeepEqual(fresh.Status.SensorStreams, desired) {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.SensorStreams = desired
		return i.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// mapSensorStreams converts the wire declaration into the CRD status shape.
func mapSensorStreams(in []*fav1.SensorStream) []fleetv1.SensorStreamEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleetv1.SensorStreamEntry, 0, len(in))
	for _, s := range in {
		if s == nil {
			continue
		}
		out = append(out, fleetv1.SensorStreamEntry{
			StreamID:             s.GetStreamId(),
			MediaType:            s.GetMediaType(),
			NativeRateHz:         s.GetNativeRateHz(),
			RequiredCapabilities: s.GetRequiredCapabilities(),
			ActivationCost:       activationCostOf(s.GetActivationCost()),
			SupportsClip:         s.GetSupportsClip(),
			ClipPreSecondsMax:    int32(s.GetClipPreSecondsMax()), //nolint:gosec // wire uint32, CRD int32; adapter-declared, not attacker-controlled magnitude
		})
	}
	return out
}

// projectHonouredOperatingConstraints writes snap.honoured_operating_constraints onto
// robot.status.honouredOperatingConstraints, structurally identical to
// projectSensorStreams above — RA-1: only on change, under retry-on-conflict.
//
// RA-1 IS THE TRAP THIS CHANGE WALKS INTO, NOT A GENERAL REMINDER. A
// CapabilitiesSnapshot arrives on a STREAM, so a projection that wrote
// unconditionally would write Robot status on a telemetry tick — the one write this
// project prohibits outright (AGENTS.md). The reflect.DeepEqual guard, and the second
// one inside the retry after the re-Get, are what make an unchanged tick free.
func (i *CapabilitiesIngestor) projectHonouredOperatingConstraints(ctx context.Context,
	robot *fleetv1.Robot, snap *fav1.CapabilitiesSnapshot,
) error {
	desired := mapHonouredOperatingConstraints(snap.GetHonouredOperatingConstraints())
	if reflect.DeepEqual(robot.Status.HonouredOperatingConstraints, desired) {
		return nil // RA-1: unchanged, no status write.
	}
	key := client.ObjectKeyFromObject(robot)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &fleetv1.Robot{}
		if err := i.Get(ctx, key, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		if reflect.DeepEqual(fresh.Status.HonouredOperatingConstraints, desired) {
			return nil
		}
		base := fresh.DeepCopy()
		fresh.Status.HonouredOperatingConstraints = desired
		return i.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// operatingSubject is the compiled form of the ONE axis-3 subject pattern
// (fleetv1.ZoneOperatingSubjectPattern). Compiled once at init: the constant is
// in-repo and MustCompile on a bad value is a build-time programming error, not a
// runtime condition on adapter input.
var operatingSubject = regexp.MustCompile(fleetv1.ZoneOperatingSubjectPattern)

// mapHonouredOperatingConstraints filters an adapter's declaration down to what the
// axis-3 vocabulary admits (ADR-0058 §7), preserving the adapter's order and
// de-duplicating.
//
// DROPPING, NOT REJECTING, AND FAIL-CLOSED EITHER WAY. The CRD carries the same
// vocabulary rule as pattern + CEL, so passing an out-of-vocabulary subject straight
// through would make ONE malformed entry fail the whole status patch and cost the
// robot its valid declarations too. A dropped subject is an UNDECLARED subject: the
// robot is withheld for it, exactly as if the adapter had stayed silent (ADR-0058
// Amendment 1 §A1.2, "an empty or absent list satisfies no subject"). Nothing here
// can widen what a robot is permitted to do.
//
// The de-duplication is not cosmetic: the field is listType=set, so a duplicate is
// refused by the API server and one repeated subject would cost the whole patch.
func mapHonouredOperatingConstraints(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if !operatingSubject.MatchString(s) {
			continue
		}
		// A bare subject must be one of the core seven; a namespaced one is open by
		// design. The pattern cannot express that split, so it is checked here — and,
		// identically, by the CEL rule on the field.
		if !strings.Contains(s, "/") && !slices.Contains(fleetv1.ZoneCoreOperatingSubjects, s) {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// activationCostOf maps the wire enum to the CRD enum. UNSPECIFIED and any
// unrecognized future value map to the empty string (unset) rather than
// guessing — this field is advisory only, so silence is the fail-safe default.
func activationCostOf(in fav1.ActivationCost) fleetv1.ActivationCost {
	switch in {
	case fav1.ActivationCost_ACTIVATION_COST_ALWAYS_ON:
		return fleetv1.ActivationCostAlwaysOn
	case fav1.ActivationCost_ACTIVATION_COST_ON_DEMAND:
		return fleetv1.ActivationCostOnDemand
	default:
		return ""
	}
}

// clock is the ingestor's time source; nil means time.Now (overridden in tests).
func (i *CapabilitiesIngestor) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// maxCustomTypeLen is spec.customType's bound, which the CRD also puts on
// status.supportedActions[].customType.
const maxCustomTypeLen = 63

// mapSupportedActions converts the wire catalog into the CRD status shape. Numeric
// min/max are intentionally not projected in v0.1 (coarse projection).
//
// custom_type (contract 1.4.0) is projected on a Custom entry only. On any other type it
// is dropped and the entry kept: the entry then reads as the type-only entry it always
// was, rather than printing a verb its type contradicts. An entry whose verb exceeds the
// bound is dropped whole: no FleetAction can name that verb, and projecting it would make
// the CRD refuse the entire status write, costing the adapter every valid entry beside
// it. Entries that share an action type are kept, in wire order: a repeated type is one
// declaration of that type, never a reason to refuse the catalog.
func mapSupportedActions(in []*fav1.SupportedAction) []fleetv1.SupportedAction {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleetv1.SupportedAction, 0, len(in))
	for _, sa := range in {
		if sa == nil {
			continue
		}
		customType := sa.GetCustomType()
		if sa.GetActionType() != string(fleetv1.ActionTypeCustom) {
			customType = ""
		}
		if len(customType) > maxCustomTypeLen {
			continue
		}
		var params []fleetv1.ActionParam
		for _, pp := range sa.GetParams() {
			if pp == nil {
				continue
			}
			params = append(params, fleetv1.ActionParam{
				Name:     pp.GetName(),
				Unit:     pp.GetUnit(),
				Kind:     pp.GetKind(),
				Allowed:  pp.GetAllowed(),
				Required: pp.GetRequired(),
			})
		}
		out = append(out, fleetv1.SupportedAction{
			ActionType:           sa.GetActionType(),
			RequiredCapabilities: sa.GetRequiredCapabilities(),
			Params:               params,
			Description:          sa.GetDescription(),
			CustomType:           customType,
		})
	}
	return out
}
