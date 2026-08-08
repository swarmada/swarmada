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

// clock is the ingestor's time source; nil means time.Now (overridden in tests).
func (i *CapabilitiesIngestor) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// mapSupportedActions converts the wire catalog into the CRD status shape. Numeric
// min/max are intentionally not projected in v0.1 (coarse projection).
func mapSupportedActions(in []*fav1.SupportedAction) []fleetv1.SupportedAction {
	if len(in) == 0 {
		return nil
	}
	out := make([]fleetv1.SupportedAction, 0, len(in))
	for _, sa := range in {
		if sa == nil {
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
		})
	}
	return out
}
