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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// §9.1.9.4 operator reset. The CLI writes the request under a SelfSubjectAccessReview; this is the
// controller half that acts on it.
//
// Two properties carry the design:
//
//   - the reset is handled BEFORE the suspension early-return. A suspended policy stops reconciling
//     everything else, so a reset processed after that guard would never run and the policy would
//     be permanently stuck — the exact failure the operator path exists to prevent.
//   - consecutiveRejections is zeroed, not just the condition. Leaving the counter at the limit
//     would re-suspend on the very next rejection, making the reset appear to do nothing.

const mpResetNS = "warehouse-a"

func policyWithSuspension(name string, rejections int32, suspended bool) *fleetv1.ModelPolicy {
	mp := &fleetv1.ModelPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mpResetNS},
		Status:     fleetv1.ModelPolicyStatus{ConsecutiveRejections: rejections},
	}
	if suspended {
		apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
			Type:    conditionFailedRepeatedly,
			Status:  metav1.ConditionTrue,
			Reason:  "ConsecutiveRejections",
			Message: "suspended",
		})
	}
	return mp
}

func newResetReconciler(t *testing.T, objs ...client.Object) (*ModelPolicyReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.ModelPolicy{}).Build()
	return &ModelPolicyReconciler{Client: c, Scheme: s}, c
}

func getMP(t *testing.T, c client.Client, name string) *fleetv1.ModelPolicy {
	t.Helper()
	var mp fleetv1.ModelPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: mpResetNS}, &mp); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	return &mp
}

func requestReset(t *testing.T, c client.Client, name, value string) {
	t.Helper()
	mp := getMP(t, c, name)
	base := mp.DeepCopy()
	if mp.Annotations == nil {
		mp.Annotations = map[string]string{}
	}
	mp.Annotations[resetAnnotation] = value
	if err := c.Patch(context.Background(), mp, client.MergeFrom(base)); err != nil {
		t.Fatalf("request reset: %v", err)
	}
}

// The reset clears the condition AND the counter.
func TestPolicyReset_ClearsSuspensionAndCounter(t *testing.T) {
	r, c := newResetReconciler(t, policyWithSuspension("nav-gate", 5, true))
	requestReset(t, c, "nav-gate", "regression fixed")

	if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
		t.Fatalf("applyPolicyReset: %v", err)
	}
	got := getMP(t, c, "nav-gate")
	if apimeta.IsStatusConditionTrue(got.Status.Conditions, conditionFailedRepeatedly) {
		t.Error("FailedRepeatedly is still True after a reset")
	}
	if got.Status.ConsecutiveRejections != 0 {
		t.Errorf("consecutiveRejections = %d after reset, want 0 — otherwise the next rejection "+
			"re-suspends immediately and the reset looks like a no-op", got.Status.ConsecutiveRejections)
	}
}

// Re-reconciling the same request writes nothing (RA-1).
func TestPolicyReset_IsIdempotent(t *testing.T) {
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(policyWithSuspension("nav-gate", 5, true)).
		WithStatusSubresource(&fleetv1.ModelPolicy{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				writes++
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &ModelPolicyReconciler{Client: c, Scheme: s}
	requestReset(t, c, "nav-gate", "once")

	for i := 0; i < 3; i++ {
		if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
			t.Fatalf("applyPolicyReset: %v", err)
		}
	}
	if writes != 1 {
		t.Errorf("status writes = %d for one reset request applied three times, want 1", writes)
	}
}

// A SECOND suspension needs a second reset: a new annotation value re-fires.
func TestPolicyReset_NewRequestFiresAgain(t *testing.T) {
	r, c := newResetReconciler(t, policyWithSuspension("nav-gate", 5, true))
	requestReset(t, c, "nav-gate", "first")
	if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
		t.Fatalf("first reset: %v", err)
	}

	// The policy suspends again.
	mp := getMP(t, c, "nav-gate")
	base := mp.DeepCopy()
	apimeta.SetStatusCondition(&mp.Status.Conditions, metav1.Condition{
		Type: conditionFailedRepeatedly, Status: metav1.ConditionTrue,
		Reason: "ConsecutiveRejections", Message: "suspended again",
	})
	mp.Status.ConsecutiveRejections = 5
	if err := c.Status().Patch(context.Background(), mp, client.MergeFrom(base)); err != nil {
		t.Fatalf("re-suspend: %v", err)
	}

	requestReset(t, c, "nav-gate", "second")
	if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if apimeta.IsStatusConditionTrue(getMP(t, c, "nav-gate").Status.Conditions, conditionFailedRepeatedly) {
		t.Error("a NEW reset request must clear a second suspension")
	}
}

// A policy that is not suspended records the request as processed but changes no status — so a
// stale annotation cannot silently clear a FUTURE suspension.
func TestPolicyReset_UnsuspendedPolicyIsMarkedNotArmed(t *testing.T) {
	r, c := newResetReconciler(t, policyWithSuspension("nav-gate", 2, false))
	requestReset(t, c, "nav-gate", "premature")

	if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
		t.Fatalf("applyPolicyReset: %v", err)
	}
	got := getMP(t, c, "nav-gate")
	if got.Annotations[resetProcessedAnnotation] != "premature" {
		t.Error("the request must be marked processed even when nothing was suspended")
	}
	if got.Status.ConsecutiveRejections != 2 {
		t.Errorf("consecutiveRejections = %d, want the untouched 2", got.Status.ConsecutiveRejections)
	}

	// Now it suspends. The already-processed annotation must NOT clear it.
	base := got.DeepCopy()
	apimeta.SetStatusCondition(&got.Status.Conditions, metav1.Condition{
		Type: conditionFailedRepeatedly, Status: metav1.ConditionTrue,
		Reason: "ConsecutiveRejections", Message: "suspended",
	})
	if err := c.Status().Patch(context.Background(), got, client.MergeFrom(base)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate")); err != nil {
		t.Fatalf("applyPolicyReset: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(getMP(t, c, "nav-gate").Status.Conditions, conditionFailedRepeatedly) {
		t.Error("a stale, already-processed reset request cleared a later suspension")
	}
}

// No request at all is a no-op.
func TestPolicyReset_NoRequestIsNoOp(t *testing.T) {
	r, c := newResetReconciler(t, policyWithSuspension("nav-gate", 5, true))
	done, err := r.applyPolicyReset(context.Background(), getMP(t, c, "nav-gate"))
	if err != nil || done {
		t.Fatalf("no reset request: done=%v err=%v, want false/nil", done, err)
	}
	if !apimeta.IsStatusConditionTrue(getMP(t, c, "nav-gate").Status.Conditions, conditionFailedRepeatedly) {
		t.Error("suspension cleared without a request")
	}
}

// THE ORDERING TEST. Everything above calls applyPolicyReset directly, so none of it proves the
// reset is reachable from Reconcile — and it only is because it runs BEFORE the suspension
// early-return. A suspended policy stops reconciling everything else, so a reset handled after that
// guard would never execute and the policy would be permanently stuck.
func TestPolicyReset_ReachableThroughReconcileWhileSuspended(t *testing.T) {
	r, c := newResetReconciler(t, policyWithSuspension("nav-gate", 5, true))
	requestReset(t, c, "nav-gate", "regression fixed")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nav-gate", Namespace: mpResetNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getMP(t, c, "nav-gate")
	if apimeta.IsStatusConditionTrue(got.Status.Conditions, conditionFailedRepeatedly) {
		t.Fatal("Reconcile left the policy suspended: the reset is unreachable behind the " +
			"suspension early-return, so an operator could never recover it")
	}
	if got.Status.ConsecutiveRejections != 0 {
		t.Errorf("consecutiveRejections = %d after reconcile, want 0", got.Status.ConsecutiveRejections)
	}
}
