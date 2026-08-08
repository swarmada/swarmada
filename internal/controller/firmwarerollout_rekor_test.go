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
	"crypto"
	"errors"
	"fmt"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

type fakeRekor struct {
	present bool
	err     error
	called  bool
	gotHash string
	gotKey  crypto.PublicKey
}

func (f *fakeRekor) HasEntry(_ context.Context, _, hash string) (bool, error) {
	f.called = true
	f.gotHash = hash
	return f.present, f.err
}

// VerifyEntry mirrors HasEntry's outcome so the existing cases keep asserting the same dispatch
// behaviour: a present entry verifies, an absent one refuses. The gotKey field records whether the
// controller passed the operator-pinned log key through, which is what separates a cryptographically
// verified check from the degraded presence-only one.
func (f *fakeRekor) VerifyEntry(_ context.Context, _, hash string, logKey crypto.PublicKey) (string, error) {
	f.called = true
	f.gotHash = hash
	f.gotKey = logKey
	if f.err != nil {
		return "", f.err
	}
	if !f.present {
		return "", fmt.Errorf("artifact %s has no entry in the transparency log", hash)
	}
	if logKey == nil {
		return "index presence only (no rekorPublicKey pinned)", nil
	}
	return "verified inclusion proof + signed entry timestamp", nil
}

func rekorConfig(url string) *fleetv1.SwarmadaConfig {
	cfg := signingConfig(true)
	cfg.Spec.Signing.RekorURL = url
	return cfg
}

// A verified artifact whose entry is in the transparency log dispatches, and Rekor
// is queried with the artifact checksum.
func TestFirmwareRollout_RekorEntryPresentDispatches(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	fr := &fakeRekor{present: true}
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), rekorConfig("https://rekor.example"), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Rekor = fr

	reconcileFirmware(t, r)

	if !robotPending(t, c, "r1") {
		t.Fatal("verified + transparency-logged artifact should dispatch")
	}
	if !fr.called || fr.gotHash != fwChecksum {
		t.Errorf("rekor not queried with the checksum: called=%v hash=%q", fr.called, fr.gotHash)
	}
}

// A verified artifact ABSENT from the transparency log fails closed.
func TestFirmwareRollout_RekorEntryAbsentFailsClosed(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), rekorConfig("https://rekor.example"), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Rekor = &fakeRekor{present: false}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("an artifact absent from the transparency log must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Error("missing transparency-log entry should fail closed")
	}
}

// A Rekor query error fails closed (never dispatch on an unverifiable log).
func TestFirmwareRollout_RekorErrorFailsClosed(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), rekorConfig("https://rekor.example"), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Rekor = &fakeRekor{err: errors.New("rekor unreachable")}

	reconcileFirmware(t, r)

	if robotPending(t, c, "r1") {
		t.Fatal("a rekor query error must not dispatch")
	}
	if getFirmwareRollout(t, c).Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Error("rekor error should fail closed")
	}
}

// With no rekorUrl configured, the transparency-log check is skipped entirely.
func TestFirmwareRollout_NoRekorURLSkipsCheck(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	fr := &fakeRekor{present: false} // would fail closed IF queried
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Rekor = fr

	reconcileFirmware(t, r)

	if !robotPending(t, c, "r1") {
		t.Fatal("no rekorUrl → the signature-verified artifact should dispatch")
	}
	if fr.called {
		t.Error("rekor must not be queried when rekorUrl is unset")
	}
}
