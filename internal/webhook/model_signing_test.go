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

// The ModelRollout signing gate at admission.
//
// Unlike the firmware gate this is NOT a duplicate of a controller check: the control plane
// never holds the model bytes and never verifies them — ModelRolloutReconciler imports no
// signing, rekor or registry package, and the artifact is verified by the Fleet Adapter on the
// robot before install. So this admission check is the only control-plane-side gate on the
// model path, and what it guarantees is exactly one thing: under a namespace policy that
// requires signature verification, a ModelRollout cannot be created with no signature
// reference for the adapter to check against. It does not verify the artifact.
//
// Every uncertainty denies, on the same fail-closed terms as the firmware gate.

func modelRollout(sigRef string) *fleetv1.ModelRollout {
	return &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "mr-1", Namespace: fwNS},
		Spec: fleetv1.ModelRolloutSpec{
			ModelName:         "item-recognition",
			NewVersion:        "3.2.1",
			ModelSignatureRef: sigRef,
		},
	}
}

func mrValidator(t *testing.T, objs ...client.Object) *ModelRolloutValidator {
	t.Helper()
	sch := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return &ModelRolloutValidator{
		Client: fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build(),
	}
}

// THE BUG. The specification stated that admission rejects a ModelRollout omitting
// modelSignatureRef; ValidateCreate was a no-op and the webhook registered DELETE only.
func TestModelSigning_UnsignedDeniedWhenEnforced(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, true))
	_, err := v.ValidateCreate(context.Background(), modelRollout(""))
	if err == nil {
		t.Fatal("SECURITY: an unsigned ModelRollout was admitted while signing is enforced")
	}
	if !strings.Contains(err.Error(), "modelSignatureRef") {
		t.Errorf("denial = %q, want it to name the missing field", err.Error())
	}
}

// Enforced + signed → admitted.
func TestModelSigning_SignedAdmitted(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateCreate(context.Background(), modelRollout("oci://reg/models/x:3.2.1.sig")); err != nil {
		t.Fatalf("a signed rollout must be admitted under enforcement, got: %v", err)
	}
}

// NOT enforced + unsigned → admitted. The gate is conditional on policy: a namespace that has
// not turned signature verification on must still be able to roll a model.
func TestModelSigning_UnsignedAdmittedWhenNotEnforced(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, false))
	if _, err := v.ValidateCreate(context.Background(), modelRollout("")); err != nil {
		t.Fatalf("unsigned must be admitted when signing is not enforced, got: %v", err)
	}
}

// No SwarmadaConfig → denied, on the same terms as the firmware gate: the policy cannot be
// determined, so the answer is refusal rather than a guess.
func TestModelSigning_AbsentConfigDenies(t *testing.T) {
	v := mrValidator(t)
	_, err := v.ValidateCreate(context.Background(), modelRollout("oci://reg/models/x:3.2.1.sig"))
	if err == nil {
		t.Fatal("with no SwarmadaConfig the policy is unknown; admission must fail closed")
	}
	if !strings.Contains(err.Error(), "fail closed") {
		t.Errorf("denial = %q, want it to state the fail-closed reason", err.Error())
	}
}

// A config-read failure → denied.
func TestModelSigning_ListErrorDenies(t *testing.T) {
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
	v := &ModelRolloutValidator{Client: c}

	if _, err := v.ValidateCreate(context.Background(), modelRollout("sig")); err == nil {
		t.Fatal("an unreadable signing policy must deny, not admit")
	}
}

// A validator with no client denies rather than silently skipping the check.
func TestModelSigning_NilClientDenies(t *testing.T) {
	v := &ModelRolloutValidator{}
	if _, err := v.ValidateCreate(context.Background(), modelRollout("")); err == nil {
		t.Fatal("a validator with no client must fail closed")
	}
}

// Update ADDING a signature ref is admitted; REMOVING it is refused. The new object is what
// the adapter will be asked to verify against.
func TestModelSigning_UpdateAddingRefAdmittedRemovingRefDenied(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateUpdate(context.Background(), modelRollout(""), modelRollout("sig")); err != nil {
		t.Fatalf("adding a signature ref must be admitted, got: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), modelRollout("sig"), modelRollout("")); err == nil {
		t.Fatal("removing the signature ref under enforcement must be refused")
	}
}

// The pre-existing delete guard must not regress now that the marker covers create;update;delete.
func TestModelSigning_DeleteGuardStillApplies(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, true))

	inFlight := modelRollout("sig")
	inFlight.Status.Phase = fleetv1.RolloutPhaseInProgress
	if _, err := v.ValidateDelete(context.Background(), inFlight); err == nil {
		t.Error("deleting a non-terminal rollout must still be refused")
	}

	done := modelRollout("sig")
	done.Status.Phase = fleetv1.RolloutPhaseSucceeded
	if _, err := v.ValidateDelete(context.Background(), done); err != nil {
		t.Errorf("deleting a terminal rollout must still be permitted, got: %v", err)
	}
}

// A non-ModelRollout object denies rather than panicking.
func TestModelSigning_WrongTypeDenies(t *testing.T) {
	v := mrValidator(t, signingConfig(fwNS, true))
	if _, err := v.ValidateCreate(context.Background(), &fleetv1.Robot{}); err == nil {
		t.Fatal("a non-ModelRollout object must be refused")
	}
}
