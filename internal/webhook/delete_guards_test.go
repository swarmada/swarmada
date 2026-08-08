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

package webhook

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// `delete` guards (docs/operations.md): a live FleetTask or a non-terminal rollout may not be deleted.
// The guard is what keeps a delete from garbage-collecting child FleetActions out from under their
// assignment leases (FleetTask) or stranding suspended model-granted capabilities (ModelRollout).

func ftPhase(name string, phase fleetv1.FleetTaskPhase) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Status:     fleetv1.FleetTaskStatus{Phase: phase},
	}
}

// A settled task — never reconciled, Pending, or terminal — is deletable.
func TestFleetTaskDelete_AllowedWhenSettled(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	for _, phase := range []fleetv1.FleetTaskPhase{
		"", // controller never reconciled it: nothing was dispatched
		fleetv1.FleetTaskPhasePending,
		fleetv1.FleetTaskPhaseSucceeded,
		fleetv1.FleetTaskPhaseFailed,
		fleetv1.FleetTaskPhaseCancelled,
		fleetv1.FleetTaskPhaseCompensated,
	} {
		if _, err := v.ValidateDelete(context.Background(), ftPhase("t", phase)); err != nil {
			t.Errorf("phase %q should be deletable, got: %v", phase, err)
		}
	}
}

// A live task — Running, or a saga still Compensating — is refused, and the message points the
// operator at the confirmed-cancel path rather than at a raw delete.
func TestFleetTaskDelete_RefusedWhenLive(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	for _, phase := range []fleetv1.FleetTaskPhase{
		fleetv1.FleetTaskPhaseRunning,
		fleetv1.FleetTaskPhaseCompensating,
	} {
		_, err := v.ValidateDelete(context.Background(), ftPhase("t1", phase))
		if err == nil {
			t.Fatalf("phase %q must not be deletable", phase)
		}
		if !apierrors.IsForbidden(err) {
			t.Errorf("phase %q: want Forbidden, got %T: %v", phase, err, err)
		}
		if !strings.Contains(err.Error(), "cancel task") {
			t.Errorf("phase %q: message should name the cancel path, got: %v", phase, err)
		}
	}
}

// A non-FleetTask object fails CLOSED: what cannot be inspected is not provably settled.
func TestFleetTaskDelete_FailsClosedOnWrongType(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	if _, err := v.ValidateDelete(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Fatal("a non-FleetTask object must be refused, not admitted")
	}
}

// ---- rollouts ----------------------------------------------------------------------------------

func fwPhase(phase fleetv1.RolloutPhase) *fleetv1.FirmwareRollout {
	return &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "fw1"},
		Status:     fleetv1.FirmwareRolloutStatus{Phase: phase},
	}
}

func mrPhase(phase fleetv1.RolloutPhase) *fleetv1.ModelRollout {
	return &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "mr1"},
		Status:     fleetv1.ModelRolloutStatus{Phase: phase},
	}
}

// Only a terminal record is deletable, for both rollout kinds.
func TestRolloutDelete_AllowedWhenTerminal(t *testing.T) {
	fw, mr := &FirmwareRolloutValidator{}, &ModelRolloutValidator{}
	for _, phase := range []fleetv1.RolloutPhase{fleetv1.RolloutPhaseSucceeded, fleetv1.RolloutPhaseFailed} {
		if _, err := fw.ValidateDelete(context.Background(), fwPhase(phase)); err != nil {
			t.Errorf("FirmwareRollout %q should be deletable, got: %v", phase, err)
		}
		if _, err := mr.ValidateDelete(context.Background(), mrPhase(phase)); err != nil {
			t.Errorf("ModelRollout %q should be deletable, got: %v", phase, err)
		}
	}
}

// A rollout that is not terminal — including Pending and an unset phase — is refused. Pending is
// included deliberately: docs/operations.md permits deleting a *terminal* record only, and the
// controller may already be selecting a batch by the time an operator's delete lands.
func TestRolloutDelete_RefusedWhenNotTerminal(t *testing.T) {
	fw, mr := &FirmwareRolloutValidator{}, &ModelRolloutValidator{}
	for _, phase := range []fleetv1.RolloutPhase{
		"",
		fleetv1.RolloutPhasePending,
		fleetv1.RolloutPhaseInProgress,
		fleetv1.RolloutPhasePaused,
	} {
		_, fwErr := fw.ValidateDelete(context.Background(), fwPhase(phase))
		if fwErr == nil || !apierrors.IsForbidden(fwErr) {
			t.Errorf("FirmwareRollout %q: want Forbidden, got %v", phase, fwErr)
		}
		_, mrErr := mr.ValidateDelete(context.Background(), mrPhase(phase))
		if mrErr == nil || !apierrors.IsForbidden(mrErr) {
			t.Errorf("ModelRollout %q: want Forbidden, got %v", phase, mrErr)
		}
	}
}

// The refusal names a mechanism that exists (`pause rollout`) and does not invent an `abandon` verb.
func TestRolloutDelete_MessageNamesOnlyRealVerbs(t *testing.T) {
	_, err := (&ModelRolloutValidator{}).ValidateDelete(context.Background(), mrPhase(fleetv1.RolloutPhaseInProgress))
	if err == nil {
		t.Fatal("an InProgress ModelRollout must not be deletable")
	}
	if !strings.Contains(err.Error(), "pause rollout") {
		t.Errorf("message should point at `swarmctl pause rollout`, got: %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "abandon") {
		t.Errorf("message must not reference an abandon verb (not implemented), got: %v", err)
	}
}

// Both rollout validators fail closed on an unexpected object type.
func TestRolloutDelete_FailsClosedOnWrongType(t *testing.T) {
	if _, err := (&FirmwareRolloutValidator{}).ValidateDelete(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Error("FirmwareRolloutValidator must refuse a non-FirmwareRollout object")
	}
	if _, err := (&ModelRolloutValidator{}).ValidateDelete(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Error("ModelRolloutValidator must refuse a non-ModelRollout object")
	}
}

// Create/update stay unconstrained on both rollout kinds — the delete guard is the whole point.
// ModelRollout create/update remain unconstrained — the CRD schema governs them and no signing gate
// applies to model artifacts at admission.
//
// FirmwareRollout create/update are NO LONGER unconstrained: they carry the signing gate
// (firmware_signing_test.go). The phase-driven part of the old assertion still holds and is kept
// here — a rollout's PHASE never constrains create/update on either kind; only the delete guard and
// the firmware signing policy do.
func TestRolloutCreateUpdate_PhaseNeverConstrains(t *testing.T) {
	mr := &ModelRolloutValidator{}
	if _, err := mr.ValidateCreate(context.Background(), mrPhase(fleetv1.RolloutPhaseInProgress)); err != nil {
		t.Errorf("ModelRollout create should be unconstrained: %v", err)
	}
	if _, err := mr.ValidateUpdate(context.Background(), mrPhase(""), mrPhase(fleetv1.RolloutPhasePaused)); err != nil {
		t.Errorf("ModelRollout update should be unconstrained: %v", err)
	}

	// FirmwareRollout: an InProgress phase is not itself a reason to refuse. With signing NOT
	// enforced the rollout is admitted regardless of phase, proving the new denial comes from the
	// signing policy and not from the phase.
	fw := fwValidator(t, signingConfig(fwNS, false))
	inFlight := fwPhase(fleetv1.RolloutPhaseInProgress)
	inFlight.Namespace = fwNS
	if _, err := fw.ValidateCreate(context.Background(), inFlight); err != nil {
		t.Errorf("FirmwareRollout create must not be constrained by phase: %v", err)
	}
	if _, err := fw.ValidateUpdate(context.Background(), fwPhase(""), inFlight); err != nil {
		t.Errorf("FirmwareRollout update must not be constrained by phase: %v", err)
	}
}
