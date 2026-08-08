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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The FirmwareRollout signing gate at admission. This DUPLICATES a rule the controller already
// enforces ("refusing to dispatch unsigned artifact"), so its whole value is that it cannot diverge
// from it: every case below is the answer the controller would give, moved earlier.
//
// Every uncertainty denies. A webhook that fails open on a config-read error would let exactly the
// object it exists to stop reach a cluster where signing is mandatory.

const fwNS = "warehouse-signing"

func signingConfig(ns string, enforce bool) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: ns},
		Spec: fleetv1.SwarmadaConfigSpec{
			Signing: fleetv1.SwarmadaSigningConfig{RequireSignatureVerification: enforce},
		},
	}
}

func rollout(sigRef string) *fleetv1.FirmwareRollout {
	return &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw-1", Namespace: fwNS},
		Spec: fleetv1.FirmwareRolloutSpec{
			FirmwareSignatureRef: sigRef,
		},
	}
}

func fwValidator(t *testing.T, objs ...client.Object) *FirmwareRolloutValidator {
	t.Helper()
	sch := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return &FirmwareRolloutValidator{
		Client: fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build(),
	}
}

// Enforced + signed → admitted.
func TestFirmwareSigning_SignedAdmitted(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateCreate(context.Background(), rollout("sig-secret")); err != nil {
		t.Fatalf("a signed rollout must be admitted under enforcement, got: %v", err)
	}
}

// Enforced + unsigned → denied, with the reason named.
func TestFirmwareSigning_UnsignedDeniedWhenEnforced(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))
	_, err := v.ValidateCreate(context.Background(), rollout(""))
	if err == nil {
		t.Fatal("an unsigned rollout must be refused while signing is enforced")
	}
	if !strings.Contains(err.Error(), "firmwareSignatureRef") {
		t.Errorf("denial = %q, want it to name the missing field", err.Error())
	}
}

// NOT enforced + unsigned → admitted. The gate is conditional on policy, not a blanket requirement:
// a namespace that has not turned on signature verification must still be able to roll firmware.
func TestFirmwareSigning_UnsignedAdmittedWhenNotEnforced(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, false))
	if _, err := v.ValidateCreate(context.Background(), rollout("")); err != nil {
		t.Fatalf("unsigned must be admitted when signing is not enforced, got: %v", err)
	}
}

// No SwarmadaConfig → denied. This MIRRORS the controller, which treats an absent config as
// "cannot determine signing policy (fail closed)". Admitting here would create a rollout the
// controller will then refuse to dispatch — a silent stall instead of a clear rejection.
func TestFirmwareSigning_AbsentConfigDenies(t *testing.T) {
	v := fwValidator(t) // no SwarmadaConfig in the namespace
	_, err := v.ValidateCreate(context.Background(), rollout("sig-secret"))
	if err == nil {
		t.Fatal("with no SwarmadaConfig the policy is unknown; admission must fail closed")
	}
	if !strings.Contains(err.Error(), "fail closed") {
		t.Errorf("denial = %q, want it to state the fail-closed reason", err.Error())
	}
}

// A config-read failure → denied. The one case where failing open would be actively dangerous.
func TestFirmwareSigning_ListErrorDenies(t *testing.T) {
	sch := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(signingConfig(fwNS, true)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("simulated API outage")
			},
		}).Build()
	v := &FirmwareRolloutValidator{Client: c}

	if _, err := v.ValidateCreate(context.Background(), rollout("sig-secret")); err == nil {
		t.Fatal("an unreadable signing policy must deny, not admit")
	}
}

// A validator with no client denies rather than silently skipping the check — a misconfigured
// wiring must not disable the gate.
func TestFirmwareSigning_NilClientDenies(t *testing.T) {
	v := &FirmwareRolloutValidator{}
	if _, err := v.ValidateCreate(context.Background(), rollout("")); err == nil {
		t.Fatal("a validator with no client must fail closed")
	}
}

// Update ADDING a signature ref is admitted — an operator must be able to repair a rollout created
// before enforcement was switched on.
func TestFirmwareSigning_UpdateAddingRefAdmitted(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateUpdate(context.Background(), rollout(""), rollout("sig-secret")); err != nil {
		t.Fatalf("adding a signature ref must be admitted, got: %v", err)
	}
}

// Update REMOVING it is refused: the new object is what will be dispatched.
func TestFirmwareSigning_UpdateRemovingRefDenied(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateUpdate(context.Background(), rollout("sig-secret"), rollout("")); err == nil {
		t.Fatal("removing the signature ref under enforcement must be refused")
	}
}

// The pre-existing delete guard must not regress now that the marker covers create;update;delete.
func TestFirmwareSigning_DeleteGuardStillApplies(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))

	inFlight := rollout("sig-secret")
	inFlight.Status.Phase = fleetv1.RolloutPhaseInProgress
	if _, err := v.ValidateDelete(context.Background(), inFlight); err == nil {
		t.Error("deleting a non-terminal rollout must still be refused")
	}

	done := rollout("sig-secret")
	done.Status.Phase = fleetv1.RolloutPhaseSucceeded
	if _, err := v.ValidateDelete(context.Background(), done); err != nil {
		t.Errorf("deleting a terminal rollout must still be permitted, got: %v", err)
	}
}

// A non-FirmwareRollout object denies rather than panicking.
func TestFirmwareSigning_WrongTypeDenies(t *testing.T) {
	v := fwValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateCreate(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Fatal("a non-FirmwareRollout object must be refused")
	}
}
