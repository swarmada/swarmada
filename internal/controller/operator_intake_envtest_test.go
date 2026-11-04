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

// The API-SERVER half of ADR-0057's operator intake.
//
// The behavioural tests for spec.desiredState (firmwarerollout_operator_hold_test.go,
// modelrollout_operator_hold_test.go, zonemaintenance_operator_close_test.go) drive the
// reconcilers against a fake client, which answers "does the controller act on the field".
// It cannot answer the questions that belong to the shipped CRD, and those are the ones a
// field like this fails on silently:
//
//   - is the field actually IN the generated CRD, or only in the Go struct? A fake client
//     round-trips a Go field whether or not `make manifests` ever emitted it.
//   - does the API server DEFAULT it, so an object stored before the field existed — and
//     any object created without it — reads back as Running/Active rather than ""?
//   - is the enum enforced, so a typo is refused at admission rather than read as "not
//     Paused" and silently ignored? That last one is the requireSignatureVerification
//     failure mode (ADR-0051, ADR-0052): a field that accepts a value and does nothing.
//
// These run against a real API server with the real CRDs (envtest).

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ── defaulting: an object created without the field acquires the default ──────────────
//
// This is also the compatibility proof for objects stored before ADR-0057: the field is
// additive with a schema default, so a stored object reads back Running/Active and takes
// exactly the branch it took before the field existed. Nothing migrates.
func TestEnvtest_OperatorIntakeDefaultsOnObjectsThatOmitIt(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fw := &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw-default", Namespace: ns},
		Spec: fleetv1.FirmwareRolloutSpec{
			TargetSelector:   metav1.LabelSelector{},
			NewVersion:       "1.9.0",
			FirmwareURI:      "oci://registry.invalid/swarmada/simbot-firmware:1.9.0",
			FirmwareChecksum: "sha256:" + strings.Repeat("a", 64),
		},
	}
	if err := envK8s.Create(ctx, fw); err != nil {
		t.Fatalf("create FirmwareRollout: %v", err)
	}
	if fw.Spec.DesiredState != fleetv1.RolloutDesiredStateRunning {
		t.Errorf("FirmwareRollout.spec.desiredState = %q, want %q from the CRD default: a stored "+
			"object that predates the field must read back Running, not \"\"",
			fw.Spec.DesiredState, fleetv1.RolloutDesiredStateRunning)
	}

	mr := &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "mr-default", Namespace: ns},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector: metav1.LabelSelector{},
			ModelName:      "item-pick",
			NewVersion:     "3.1.0",
			ModelURI:       "oci://registry.invalid/swarmada/item-pick:3.1.0",
			ModelChecksum:  "sha256:" + strings.Repeat("b", 64),
		},
	}
	if err := envK8s.Create(ctx, mr); err != nil {
		t.Fatalf("create ModelRollout: %v", err)
	}
	if mr.Spec.DesiredState != fleetv1.RolloutDesiredStateRunning {
		t.Errorf("ModelRollout.spec.desiredState = %q, want %q from the CRD default",
			mr.Spec.DesiredState, fleetv1.RolloutDesiredStateRunning)
	}

	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "zm-default", Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		},
	}
	if err := envK8s.Create(ctx, zm); err != nil {
		t.Fatalf("create ZoneMaintenance: %v", err)
	}
	if zm.Spec.DesiredState != fleetv1.ZoneMaintenanceDesiredStateActive {
		t.Errorf("ZoneMaintenance.spec.desiredState = %q, want %q from the CRD default",
			zm.Spec.DesiredState, fleetv1.ZoneMaintenanceDesiredStateActive)
	}
	// And the default is Active, not Completed: acquiring the field must not close a window.
	if zm.Spec.DesiredState == fleetv1.ZoneMaintenanceDesiredStateCompleted {
		t.Fatal("a ZoneMaintenance created without desiredState defaulted to Completed")
	}
}

// ── the enum is enforced by the API server, not merely documented ─────────────────────
//
// A value outside the enum must be REFUSED. If it were accepted, the controller's
// `== Paused` comparison would read it as "not paused" and the operator's intent would be
// silently discarded — which is exactly the defect class ADR-0057 says not to reproduce.
func TestEnvtest_OperatorIntakeRejectsAValueOutsideTheEnum(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fw := &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw-bad-enum", Namespace: ns},
		Spec: fleetv1.FirmwareRolloutSpec{
			TargetSelector:   metav1.LabelSelector{},
			NewVersion:       "1.9.0",
			FirmwareURI:      "oci://registry.invalid/swarmada/simbot-firmware:1.9.0",
			FirmwareChecksum: "sha256:" + strings.Repeat("a", 64),
			// "Stopped" is a verb an operator might reasonably reach for and this field
			// does not carry: it must be refused, not stored and ignored.
			DesiredState: fleetv1.RolloutDesiredState("Stopped"),
		},
	}
	err := envK8s.Create(ctx, fw)
	if err == nil {
		t.Fatal("the API server accepted spec.desiredState: Stopped on a FirmwareRollout; " +
			"an unsupported intent must be refused at admission, never stored and read as 'not Paused'")
	}
	if !strings.Contains(err.Error(), "desiredState") {
		t.Errorf("rejection does not name the field: %v", err)
	}

	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "zm-bad-enum", Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
			// `Cancelled` is a PHASE this type declares and no code writes; it is deliberately
			// not a desiredState value, and must be refused rather than stored.
			DesiredState: fleetv1.ZoneMaintenanceDesiredState("Cancelled"),
		},
	}
	if err := envK8s.Create(ctx, zm); err == nil {
		t.Fatal("the API server accepted spec.desiredState: Cancelled on a ZoneMaintenance")
	}
}

// ── the operator close, end to end through the real CRD ───────────────────────────────
//
// The same assertion as TestZoneMaintenance_OperatorCloseReachesCompletedWithoutTheDeadline,
// carried out against a real API server: the window is created with the default
// autoResumeAfterMinutes (0 — no deadline exists to reach Completed by), activated, and then
// closed by an operator writing spec.desiredState through the API. The robot it paused must
// be back in service afterwards, which is the property that makes this a close rather than a
// phase write.
func TestEnvtest_OperatorCloseReachesCompletedThroughTheAPIServer(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns},
		Spec: fleetv1.RobotSpec{
			Zone: "z-a", Manufacturer: "acme", Model: "amr-100",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
		},
	}
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.CurrentZone = "z-a"
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "early-close", Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
			Mode:  fleetv1.ZoneMaintenanceModeImmediate,
			// The default, and the whole point: no deadline exists to reach Completed by.
			AutoResumeAfterMinutes: 0,
		},
	}
	if err := envK8s.Create(ctx, zm); err != nil {
		t.Fatalf("create ZoneMaintenance: %v", err)
	}

	r := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme}
	key := types.NamespacedName{Name: "early-close", Namespace: ns}
	req := ctrl.Request{NamespacedName: key}
	for i := 0; i < 2; i++ { // finalizer, then activate
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	var got fleetv1.ZoneMaintenance
	if err := envK8s.Get(ctx, key, &got); err != nil {
		t.Fatalf("get zm: %v", err)
	}
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("phase = %s, want Active before the close", got.Status.Phase)
	}
	if got.Status.AutoResumeAt != nil {
		t.Fatalf("autoResumeAt = %v, want nil: with no deadline, nothing but an operator could "+
			"ever close this window", got.Status.AutoResumeAt)
	}
	var paused fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "amr-1", Namespace: ns}, &paused); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if paused.Status.Phase != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s, want Maintenance (the window should have paused it)", paused.Status.Phase)
	}

	// ── the operator closes it, through the API server ───────────────────────────────
	got.Spec.DesiredState = fleetv1.ZoneMaintenanceDesiredStateCompleted
	if err := envK8s.Update(ctx, &got); err != nil {
		t.Fatalf("operator close (spec update): %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile after close: %v", err)
	}

	if err := envK8s.Get(ctx, key, &got); err != nil {
		t.Fatalf("get zm after close: %v", err)
	}
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed after an operator close with no deadline", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Error("completedAt was not stamped; the window records no closure time")
	}
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "amr-1", Namespace: ns}, &paused); err != nil {
		t.Fatalf("get robot after close: %v", err)
	}
	if paused.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle: a close that leaves a robot in Maintenance has "+
			"stranded it, which is worse than no close at all", paused.Status.Phase)
	}
}
