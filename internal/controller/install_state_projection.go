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
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Projection of ADAPTER-REPORTED install state onto Robot.status (ADR-0033).
//
// Before this, Robot.status.installedModels[] held only the control plane's own record of
// what it had pushed — written exclusively with Updating — so a rollout could never observe
// an install finishing. It could not complete, and the two model audit rows could not fire.
// There was no firmware equivalent at all.
//
// Two carriers land here so they cannot diverge:
//
//   - the terminal UpdateProgress, for timeliness; and
//   - CapabilitiesSnapshot, for recovery when the stream dropped or the control plane
//     restarted mid-install.
//
// Both are event- or scan-driven, never per telemetry tick, so RA-1 holds; each write is
// additionally suppressed when nothing changed.

// findRobotByIDInNamespace resolves a wire robot_id to a Robot WITHIN one namespace.
//
// The namespace scope is required, not an optimisation. The robot-id annotation is enforced
// unique per namespace (robot_webhook.go), so two namespaces may each legitimately hold a
// robot with the same id — a cluster-wide lookup sees two matches and refuses as though the
// state were invalid, breaking projection for both. Every ingestor knows the namespace the
// message arrived on, so there is no reason to search outside it.
func findRobotByIDInNamespace(ctx context.Context, c client.Client, namespace, robotID string) (*fleetv1.Robot, error) {
	if robotID == "" || namespace == "" {
		return nil, nil
	}
	var list fleetv1.RobotList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing robots in %q to resolve robot_id %q: %w", namespace, robotID, err)
	}
	var match *fleetv1.Robot
	for i := range list.Items {
		if list.Items[i].Annotations[RobotIDAnnotation] != robotID {
			continue
		}
		if match != nil {
			// Within ONE namespace this is a genuine spec violation, so refusing to guess
			// is right here even though it was wrong cluster-wide.
			return nil, fmt.Errorf("robot_id %q maps to more than one Robot in namespace %q", robotID, namespace)
		}
		match = &list.Items[i]
	}
	return match, nil
}

// applyInstallState patches Robot.status with a mutation, but only if it changes something.
//
// The no-op comparison is done against a FRESHLY read object inside the retry, not against
// the caller's copy: two carriers report the same install, so the common case is a redundant
// report arriving while the other has already been written.
func applyInstallState(ctx context.Context, c client.Client, key client.ObjectKey,
	mutate func(*fleetv1.Robot) bool,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &fleetv1.Robot{}
		if err := c.Get(ctx, key, fresh); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := fresh.DeepCopy()
		if !mutate(fresh) {
			return nil
		}
		if reflect.DeepEqual(base.Status, fresh.Status) {
			return nil // RA-1: nothing actually changed.
		}
		return c.Status().Patch(ctx, fresh, client.MergeFrom(base))
	})
}

// projectFirmwareState writes a reported firmware install state onto the robot.
func projectFirmwareState(ctx context.Context, c client.Client, key client.ObjectKey,
	state *fleetv1.FirmwareInstallState,
) error {
	if state == nil {
		return nil
	}
	return applyInstallState(ctx, c, key, func(rob *fleetv1.Robot) bool {
		// ReportedAt alone must not trigger a write, or every redundant report becomes a
		// status write and RA-1 is lost by the back door. Compare the substance first.
		if cur := rob.Status.FirmwareInstall; cur != nil &&
			cur.Status == state.Status &&
			cur.RunningVersion == state.RunningVersion &&
			cur.AttemptedVersion == state.AttemptedVersion &&
			cur.FailureReason == state.FailureReason {
			return false
		}
		rob.Status.FirmwareInstall = state
		return true
	})
}

// projectModelState writes one reported model's state onto the robot, upserting by name.
func projectModelState(ctx context.Context, c client.Client, key client.ObjectKey,
	entry fleetv1.InstalledModelStatusEntry,
) error {
	if entry.Name == "" {
		return nil
	}
	return applyInstallState(ctx, c, key, func(rob *fleetv1.Robot) bool {
		for i := range rob.Status.InstalledModels {
			e := &rob.Status.InstalledModels[i]
			if e.Name != entry.Name {
				continue
			}
			if e.Status == entry.Status && e.RunningVersion == entry.RunningVersion &&
				e.FailureReason == entry.FailureReason {
				return false
			}
			e.Status = entry.Status
			e.RunningVersion = entry.RunningVersion
			e.FailureReason = entry.FailureReason
			// ActiveAt marks the last transition INTO Active and is not rewritten while the
			// model stays Active, so it measures how long the model has been serving.
			if entry.Status == fleetv1.ModelStatusActive && e.ActiveAt == nil {
				e.ActiveAt = entry.ActiveAt
			}
			if entry.Status != fleetv1.ModelStatusActive {
				e.ActiveAt = nil
			}
			return true
		}
		rob.Status.InstalledModels = append(rob.Status.InstalledModels, entry)
		return true
	})
}

// firmwareStateFromSnapshot maps the wire firmware state, or nil when the adapter reported
// none. Nil is meaningful: it leaves Robot.status.firmwareInstall absent, keeping "not
// reported" distinct from "reported as something".
func firmwareStateFromSnapshot(fs *fav1.FirmwareState, now time.Time) *fleetv1.FirmwareInstallState {
	if fs == nil {
		return nil
	}
	status := firmwareStatusFromWire(fs.GetStatus())
	if status == "" {
		return nil // UNSPECIFIED: the adapter is not reporting, not reporting "nothing".
	}
	return &fleetv1.FirmwareInstallState{
		Status:           status,
		RunningVersion:   fs.GetRunningVersion(),
		AttemptedVersion: fs.GetAttemptedVersion(),
		FailureReason:    fs.GetFailureReason(),
		ReportedAt:       &metav1.Time{Time: now},
	}
}

func firmwareStatusFromWire(s fav1.FirmwareInstallStatus) fleetv1.FirmwareInstallStatus {
	switch s {
	case fav1.FirmwareInstallStatus_FIRMWARE_INSTALL_STATUS_RUNNING:
		return fleetv1.FirmwareInstallRunning
	case fav1.FirmwareInstallStatus_FIRMWARE_INSTALL_STATUS_UPDATING:
		return fleetv1.FirmwareInstallUpdating
	case fav1.FirmwareInstallStatus_FIRMWARE_INSTALL_STATUS_FAILED:
		return fleetv1.FirmwareInstallFailed
	default:
		return "" // UNSPECIFIED, or a value from a newer contract this build cannot read.
	}
}

// modelEntryFromSnapshot maps one reported model. Returns ok=false for an unusable entry
// rather than writing a half-formed one.
func modelEntryFromSnapshot(m *fav1.InstalledModel) (fleetv1.InstalledModelStatusEntry, bool) {
	if m == nil || m.GetName() == "" {
		return fleetv1.InstalledModelStatusEntry{}, false
	}
	status := modelStatusFromWire(m.GetStatus())
	if status == "" {
		return fleetv1.InstalledModelStatusEntry{}, false
	}
	return fleetv1.InstalledModelStatusEntry{
		Name:           m.GetName(),
		Status:         status,
		RunningVersion: m.GetRunningVersion(),
		FailureReason:  m.GetFailureReason(),
	}, true
}

func modelStatusFromWire(s fav1.ModelStatus) fleetv1.ModelStatus {
	switch s {
	case fav1.ModelStatus_MODEL_STATUS_ACTIVE:
		return fleetv1.ModelStatusActive
	case fav1.ModelStatus_MODEL_STATUS_UPDATING:
		return fleetv1.ModelStatusUpdating
	case fav1.ModelStatus_MODEL_STATUS_FAILED:
		return fleetv1.ModelStatusFailed
	case fav1.ModelStatus_MODEL_STATUS_INACTIVE:
		return fleetv1.ModelStatusInactive
	default:
		return ""
	}
}
