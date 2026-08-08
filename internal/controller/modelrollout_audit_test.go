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
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
)

// ModelRollout producers for the §9.6.5.1 chain.
//
// Model signature evidence differs in kind from firmware's: the control plane verifies a
// firmware artifact itself before dispatch, but never sees a model's bytes — the adapter
// verifies and attests on the model_update acknowledgement. These entries therefore record
// what the robot attested, which is the only evidence available, and the tests care that
// the distinction is not blurred.

// signerPusher returns a scripted acknowledgement, optionally with a signer or a decline
// message, so each reply shape can be exercised independently.
type signerPusher struct {
	ack     bool
	signer  string
	message string
}

func (p *signerPusher) PushModelUpdate(_ context.Context, _, _ string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	return command.ModelUpdateOutcome{
		Acknowledged: p.ack, VerifiedSigner: p.signer, Message: p.message,
	}, nil
}

func TestAuditMR_UpdateStartedAndSignatureVerified(t *testing.T) {
	rec := &recordingAudit{}
	r, _ := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = &signerPusher{ack: true, signer: "CN=Swarmada Model Signing"}
	r.Audit = rec

	reconcileRollout(t, r)

	started := rec.ofType(audit.EventModelUpdateStarted)
	if len(started) != 1 {
		t.Fatalf("want one MODEL_UPDATE_STARTED, got %d", len(started))
	}
	if started[0].Detail["model_name"] == "" || started[0].Detail["new_version"] == "" {
		t.Errorf("model_name/new_version are required detail fields: %v", started[0].Detail)
	}

	verified := rec.ofType(audit.EventModelSignatureVerified)
	if len(verified) != 1 {
		t.Fatalf("want one MODEL_SIGNATURE_VERIFIED, got %d", len(verified))
	}
	// The signer is the adapter's attestation of which trust root it checked against, and
	// is the whole content of the evidence for a model.
	if verified[0].Detail["verified_signer"] != "CN=Swarmada Model Signing" {
		t.Errorf("verified_signer not carried through: %q", verified[0].Detail["verified_signer"])
	}
	if verified[0].Outcome != audit.OutcomeAllowed {
		t.Errorf("a successful verification is Allowed, got %q", verified[0].Outcome)
	}

	// The model is now Updating, which keeps the robot out of a later batch, so neither
	// entry can repeat while the update is in flight (RA-1).
	reconcileRollout(t, r)
	reconcileRollout(t, r)
	if n := len(rec.ofType(audit.EventModelUpdateStarted)); n != 1 {
		t.Fatalf("STARTED must seal once per robot per rollout, got %d", n)
	}
	if n := len(rec.ofType(audit.EventModelSignatureVerified)); n != 1 {
		t.Fatalf("VERIFIED must seal once per robot per rollout, got %d", n)
	}
}

func TestAuditMR_SignatureFailureIsSealed(t *testing.T) {
	// §9.2.8 requires a fail-closed signature refusal to decline with this reason.
	rec := &recordingAudit{}
	r, _ := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = &signerPusher{ack: false, message: "SignatureVerificationFailed: trust root mismatch"}
	r.Audit = rec

	reconcileRollout(t, r)

	got := rec.ofType(audit.EventModelSignatureFailed)
	if len(got) != 1 {
		t.Fatalf("want one MODEL_SIGNATURE_FAILED, got %d", len(got))
	}
	if got[0].Outcome != audit.OutcomeDenied {
		t.Errorf("a refused artifact is Denied, got %q", got[0].Outcome)
	}
	for _, f := range []string{"model_name", "reason"} {
		if got[0].Detail[f] == "" {
			t.Errorf("required detail field %q missing: %v", f, got[0].Detail)
		}
	}
	// A refused artifact was never installed, so nothing may claim the update started.
	if n := len(rec.ofType(audit.EventModelUpdateStarted)); n != 0 {
		t.Fatalf("a declined push must not record an update start, got %d", n)
	}
}

func TestAuditMR_OrdinaryDeclineIsNotASignatureFailure(t *testing.T) {
	// A decline for any other cause is a delivery problem. Recording it as a signature
	// failure would put a false rejection in the chain and implicate an artifact that was
	// never actually refused on its signature.
	rec := &recordingAudit{}
	r, _ := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = &signerPusher{ack: false, message: "robot busy; retry later"}
	r.Audit = rec

	reconcileRollout(t, r)
	if n := len(rec.ofType(audit.EventModelSignatureFailed)); n != 0 {
		t.Fatalf("a non-signature decline must not seal MODEL_SIGNATURE_FAILED, got %d", n)
	}
}

func TestAuditMR_UpdateSucceededOnlyForRobotsThisRolloutUpdated(t *testing.T) {
	// A robot already running the target version installed nothing; recording a success
	// for it would inflate the rollout's evidence with work that never happened.
	rec := &recordingAudit{}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", RunningVersion: "3.2.1", Status: fleetv1.ModelStatusActive,
	}}
	r, _ := newRolloutReconciler(t, pickerRollout(), rob)
	r.Audit = rec

	reconcileRollout(t, r)
	if n := len(rec.ofType(audit.EventModelUpdateSucceeded)); n != 0 {
		t.Fatalf("a robot already on the target version must not record a success, got %d", n)
	}
}

func TestAuditMR_UpdateFailedSealsOnceOnTheEdge(t *testing.T) {
	// A failed model stays Failed across reconciles. Without the failedRobots diff the
	// chain would gain one entry per reconcile per robot and bury the incident it records.
	rec := &recordingAudit{}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", RunningVersion: "3.0.0",
		Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed",
	}}
	r, _ := newRolloutReconciler(t, pickerRollout(), rob)
	r.Audit = rec

	reconcileRollout(t, r)
	got := rec.ofType(audit.EventModelUpdateFailed)
	if len(got) != 1 {
		t.Fatalf("want one MODEL_UPDATE_FAILED, got %d", len(got))
	}
	if got[0].Outcome != audit.OutcomeError {
		t.Errorf("a failed install is an Error outcome, got %q", got[0].Outcome)
	}
	if got[0].Detail["reason"] != "inference health check failed" {
		t.Errorf("the adapter's reason must be carried through, got %q", got[0].Detail["reason"])
	}

	reconcileRollout(t, r)
	reconcileRollout(t, r)
	if n := len(rec.ofType(audit.EventModelUpdateFailed)); n != 1 {
		t.Fatalf("FAILED must seal once per robot, got %d entries", n)
	}
}

func TestAuditMR_NilRecorderIsSafe(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = &signerPusher{ack: true}
	// r.Audit deliberately nil.
	reconcileRollout(t, r)
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatal("nil Audit must not change the rollout's behaviour")
	}
}
