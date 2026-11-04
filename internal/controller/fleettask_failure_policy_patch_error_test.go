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
	"errors"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A FailFast task cancels its running members before it is Failed. A terminal task is never
// reconciled again, so a member whose cancel was refused and dropped kept running under a task
// that read Failed, and a later cancel of the task reached nothing. The refusal is returned and
// the task stays non-terminal; the next pass cancels the member and then fails the task.
func TestFleetTask_FailFastDoesNotEndTheTaskBeforeItsMembersAreCancelled(t *testing.T) {
	ft := fleetTask("ft-ff", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	var refuse atomic.Bool
	refuse.Store(true)
	member := childName("ft-ff", "b")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ft, childAction("ft-ff", "a", fleetv1.ActionPhaseFailed), childAction("ft-ff", "b", fleetv1.ActionPhaseInProgress)).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption) error {
			if fa, ok := obj.(*fleetv1.FleetAction); ok && fa.Name == member && fa.Spec.DesiredState == fleetv1.DesiredStateCancelled && refuse.Load() {
				return errors.New("the API server refused the write")
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}}).
		Build()
	r := &FleetTaskReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: "ft-ff"}}

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Errorf("a member the failure policy could not cancel was not reported")
	}
	if got := getFT(t, c, "ft-ff").Status.Phase; isGroupTerminal(got) {
		t.Fatalf("the task is %q while its member %s runs uncancelled", got, member)
	}

	refuse.Store(false)
	reconcileFT(t, r, "ft-ff")
	var b fleetv1.FleetAction
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: member}, &b); err != nil {
		t.Fatalf("get member: %v", err)
	}
	if b.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("the member's desiredState is %q after the retry, not Cancelled", b.Spec.DesiredState)
	}
	if got := getFT(t, c, "ft-ff").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("the task is %q after its member was cancelled, not Failed", got)
	}
}
