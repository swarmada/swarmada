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

package heldwork_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// garbageCollector is the garbage collector where each controller runs under its own ServiceAccount.
var garbageCollector = identity{user: "system:serviceaccount:kube-system:generic-garbage-collector", groups: []string{
	"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}}

// heldOn gives robot a stop of its own and makes it record action, which then holds the stop
// finalizer, as the control plane would. ownerTask, when set, makes action a member of that task.
func (r *row) heldOn(robot, action, ownerTask string) {
	r.t.Helper()
	ctx := context.Background()
	mgr := r.as(r.manager())
	fa := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: action, Namespace: r.ns},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
	if ownerTask != "" {
		yes := true
		fa.OwnerReferences = []metav1.OwnerReference{{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetTask",
			Name: ownerTask, UID: types.UID("uid-" + ownerTask), Controller: &yes, BlockOwnerDeletion: &yes}}
	}
	if err := mgr.Create(ctx, fa); err != nil {
		r.t.Fatalf("creating action %s: %v", action, err)
	}
	// The Robot webhook reads the adapter through the manager's cache, which can lag its status.
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		rb := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: robot, Namespace: r.ns,
			Annotations: map[string]string{estopscope.TriggerAnnotation: "held-work-row"}},
			Spec: fleetv1.RobotSpec{Manufacturer: "acme-robotics", Model: "acme-picker", Zone: "bay-1",
				Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}}}
		if err = rigAdmin.Create(ctx, rb); err == nil || !strings.Contains(err.Error(), "is not ready") || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatalf("creating robot %s under its own stop: %v", robot, err)
	}
	rb := &fleetv1.Robot{}
	if err := mgr.Get(ctx, client.ObjectKey{Namespace: r.ns, Name: robot}, rb); err != nil {
		r.t.Fatal(err)
	}
	rb.Status.AssignedAction = action
	if err := mgr.Status().Update(ctx, rb); err != nil {
		r.t.Fatalf("recording %s on %s: %v", action, robot, err)
	}
	if err := mgr.Get(ctx, client.ObjectKeyFromObject(fa), fa); err != nil {
		r.t.Fatal(err)
	}
	controllerutil.AddFinalizer(fa, estopscope.StopFinalizer)
	if err := mgr.Update(ctx, fa); err != nil {
		r.t.Fatalf("adding the stop finalizer to %s: %v", action, err)
	}
}

// heldWorkFixture creates the zone and the Connected, conformance-Passed adapter a Robot needs.
func (r *row) heldWorkFixture() {
	r.t.Helper()
	ctx := context.Background()
	if err := rigAdmin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-1", Namespace: r.ns}}); err != nil {
		r.t.Fatalf("creating the zone: %v", err)
	}
	adapter := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: r.ns},
		Spec: fleetv1.FleetAdapterSpec{Vendor: "acme-robotics"}}
	if err := rigAdmin.Create(ctx, adapter); err != nil {
		r.t.Fatalf("creating the adapter: %v", err)
	}
	adapter.Status = fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected,
		Conformance: fleetv1.ConformanceStatePassed, ConformanceContractVersion: contract.Version}
	if err := rigAdmin.Status().Update(ctx, adapter); err != nil {
		r.t.Fatalf("marking the adapter Connected and conformance-Passed: %v", err)
	}
}

// TestHeldWork_AServiceAccountNeverMovesHeldWork: a ServiceAccount bound to swarmada:admin holds
// estop-clear by RBAC, and still neither deletes held work nor removes its stop finalizer. The
// garbage collector deletes held work only in a cascade from a FleetTask, which the finalizer then
// holds. A person bound to the same role deletes it, and the finalizer keeps it (RFC-0001
// safety.md, Rules for every A row; Held work and who decides).
func TestHeldWork_AServiceAccountNeverMovesHeldWork(t *testing.T) {
	r := newRow(t)
	ctx := context.Background()
	r.bind(r.manager(), "swarmada-manager-role")
	sa := r.serviceAccount("dispatcher")
	r.bind(sa, "swarmada:admin")
	admin := r.user("admin")
	r.bind(admin, "swarmada:admin")
	r.heldWorkFixture()
	r.heldOn("r1", "x", "")
	r.heldOn("r2", "y", "dock")
	r.heldOn("r3", "z", "")

	action := func(name string) *fleetv1.FleetAction {
		return &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.ns}}
	}
	kept := func(name string) *fleetv1.FleetAction {
		t.Helper()
		fa := &fleetv1.FleetAction{}
		if err := rigAdmin.Get(ctx, client.ObjectKey{Namespace: r.ns, Name: name}, fa); err != nil {
			t.Fatalf("INVARIANT: held work %s is gone while its robot's stop stands: %v", name, err)
		}
		return fa
	}
	refused := func(what string, err error) {
		t.Helper()
		t.Logf("RESULT %s: %v", what, err)
		if err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("INVARIANT: %s was not refused: %v", what, err)
		} else if !strings.Contains(err.Error(), "ServiceAccount") {
			t.Errorf("%s was refused, but not as a ServiceAccount: %v", what, err)
		}
	}

	// The ServiceAccount holds estop-clear by RBAC, so these refusals are the identity's.
	fa := kept("x")
	controllerutil.RemoveFinalizer(fa, estopscope.StopFinalizer)
	refused("the ServiceAccount removes x's stop finalizer", r.as(sa).Update(ctx, fa))
	refused("the ServiceAccount deletes x", r.as(sa).Delete(ctx, action("x")))
	refused("the garbage collector deletes z, a member of no task", r.as(garbageCollector).Delete(ctx, action("z")))
	if fa := kept("x"); fa.DeletionTimestamp != nil || !controllerutil.ContainsFinalizer(fa, estopscope.StopFinalizer) {
		t.Errorf("INVARIANT: a refused request changed x: deleting=%v finalizers %v", fa.DeletionTimestamp != nil, fa.Finalizers)
	}
	if fa := kept("z"); fa.DeletionTimestamp != nil {
		t.Errorf("INVARIANT: the refused delete of z was carried out")
	}

	// The controls: the garbage collector's cascade from a FleetTask, and a person with estop-clear.
	// Each delete is admitted, and the stop finalizer keeps the work.
	for _, c := range []struct {
		who  string
		id   identity
		name string
	}{{"the garbage collector deletes y, a member of task dock", garbageCollector, "y"}, {"the admin deletes x", admin, "x"}} {
		err := r.as(c.id).Delete(ctx, action(c.name))
		t.Logf("RESULT %s: %v", c.who, err)
		if err != nil {
			t.Errorf("control: %s was refused: %v", c.who, err)
			continue
		}
		if fa := kept(c.name); fa.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(fa, estopscope.StopFinalizer) {
			t.Errorf("INVARIANT: %s: want Terminating with the stop finalizer, got deleting=%v finalizers %v",
				c.who, fa.DeletionTimestamp != nil, fa.Finalizers)
		}
	}
}
