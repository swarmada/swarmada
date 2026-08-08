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
	"errors"
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// The first (verified) reconcile of a FirmwareRollout seals exactly one
// FIRMWARE_ROLLOUT_CREATED entry; subsequent reconciles do not re-record it.
func TestFirmwareRollout_CreationIsAuditedOnce(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Audit = rec

	// Counted BY TYPE, not by total: this reconcile now also seals
	// FIRMWARE_SIGNATURE_VERIFIED and FIRMWARE_INSTALL_STARTED, and a bare length check
	// would fail whenever a new producer is added rather than when this one misbehaves.
	reconcileFirmware(t, r) // phase ""→set: records
	created := entriesOfType(rec, audit.EventFirmwareRolloutCreat)
	if len(created) != 1 {
		t.Fatalf("FIRMWARE_ROLLOUT_CREATED entries = %d, want exactly 1", len(created))
	}
	e := created[0]
	if e.Resource.Kind != "FirmwareRollout" || e.Actor.Type != audit.ActorServiceAccount {
		t.Errorf("resource/actor = %+v / %+v", e.Resource, e.Actor)
	}

	reconcileFirmware(t, r) // phase already set: must NOT re-record
	if n := len(entriesOfType(rec, audit.EventFirmwareRolloutCreat)); n != 1 {
		t.Errorf("FIRMWARE_ROLLOUT_CREATED re-recorded on a later reconcile: %d entries", n)
	}
}

// entriesOfType filters a captureRecorder's entries by event type. Assertions about one
// producer must not be coupled to how many other producers happen to exist.
func entriesOfType(rec *captureRecorder, eventType string) []audit.Entry {
	var out []audit.Entry
	for _, e := range rec.entries {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// ── §9.6.5.1 firmware producers ────────────────────────────────────────────────────────
//
// Signature outcomes are the most safety-relevant records this specification defines: they
// are the evidence that unverified code was never dispatched. Until now the failure case
// was a Kubernetes Event only — subject to namespace retention, outside the hash chain, and
// therefore unusable as evidence. That gap is what these close.

func TestAuditFW_SignatureVerified_SealsOnceWithSigner(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Audit = rec

	reconcileFirmware(t, r)
	got := entriesOfType(rec, audit.EventFirmwareSignatureVerified)
	if len(got) != 1 {
		t.Fatalf("want exactly one FIRMWARE_SIGNATURE_VERIFIED, got %d", len(got))
	}
	e := got[0]
	if e.Outcome != audit.OutcomeAllowed {
		t.Errorf("a successful verification is Allowed, got %q", e.Outcome)
	}
	for _, f := range []string{"firmware_uri", "artifact_digest", "verified_signer"} {
		if e.Detail[f] == "" {
			t.Errorf("required detail field %q missing: %v", f, e.Detail)
		}
	}
	if e.Detail["artifact_digest"] != fwChecksum {
		t.Errorf("artifact_digest must be the dispatched checksum, got %q", e.Detail["artifact_digest"])
	}

	// The condition is re-asserted on every reconcile; only its edge is an event.
	reconcileFirmware(t, r)
	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareSignatureVerified)); n != 1 {
		t.Fatalf("VERIFIED must seal once per rollout, got %d", n)
	}
}

func TestAuditFW_SignatureFailed_IsSealedIntoTheChainNotOnlyEvented(t *testing.T) {
	// A signature that does not verify must leave chain evidence that dispatch was refused.
	secret, sigRef := signingFixture(t, "sha256:"+strings.Repeat("f", 64)) // signs a DIFFERENT artifact
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Audit = rec

	reconcileFirmware(t, r)
	got := entriesOfType(rec, audit.EventFirmwareSignatureFailed)
	if len(got) != 1 {
		t.Fatalf("want exactly one FIRMWARE_SIGNATURE_FAILED in the chain, got %d", len(got))
	}
	e := got[0]
	// Denied, not Allowed: the entry records a refusal, and an auditor filtering on outcome
	// must be able to find it.
	if e.Outcome != audit.OutcomeDenied {
		t.Errorf("a refused artifact is Denied, got %q", e.Outcome)
	}
	for _, f := range []string{"firmware_uri", "artifact_digest", "reason"} {
		if e.Detail[f] == "" {
			t.Errorf("required detail field %q missing: %v", f, e.Detail)
		}
	}
	// And nothing was dispatched — the record must not be able to claim otherwise.
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallStarted)); n != 0 {
		t.Fatalf("a failed verification must dispatch nothing, got %d INSTALL_STARTED", n)
	}
}

func TestAuditFW_InstallStarted_RecordsBothVersions(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	rec := &captureRecorder{}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.FirmwareVersion = "1.0.0"
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret, rob)
	r.Audit = rec

	reconcileFirmware(t, r)
	got := entriesOfType(rec, audit.EventFirmwareInstallStarted)
	if len(got) != 1 {
		t.Fatalf("want one FIRMWARE_INSTALL_STARTED, got %d", len(got))
	}
	// old_version is the point: it is what the robot falls back to if the install fails,
	// and it cannot be recovered from the object once the update lands.
	if got[0].Detail["old_version"] != "1.0.0" {
		t.Errorf("old_version = %q, want the version the robot was on", got[0].Detail["old_version"])
	}
	if got[0].Detail["new_version"] == "" {
		t.Error("new_version is a required detail field")
	}

	// The pending-firmware annotation now excludes the robot from dispatch, so the edge
	// cannot re-fire while the update is in flight.
	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallStarted)); n != 1 {
		t.Fatalf("INSTALL_STARTED must seal once per robot per rollout, got %d", n)
	}
}

func TestAuditFW_NilRecorderAndSinkFailureAreSafe(t *testing.T) {
	// Neither a nil recorder nor a failing sink may change what the rollout does — least
	// of all on the verification path, where refusing to dispatch is the safe behaviour.
	secret, sigRef := signingFixture(t, fwChecksum)
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	reconcileFirmware(t, r) // r.Audit nil

	r2, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r2.Audit = &failingRecorder{}
	reconcileFirmware(t, r2)
}

type failingRecorder struct{}

func (failingRecorder) Record(audit.Entry) (audit.Entry, error) {
	return audit.Entry{}, errors.New("sink unavailable")
}
