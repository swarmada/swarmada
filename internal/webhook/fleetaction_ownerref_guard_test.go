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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Only the control plane sets or changes a FleetAction's owner references. A FleetTask's
// controller reference is removed only by the control plane or the garbage collector, which
// removes it when it orphans a deleted task's children; any other owner reference may be
// removed by anyone.

func ownedAction(refs ...metav1.OwnerReference) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "dock-sweep", OwnerReferences: refs},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}
}

func taskRef(name, uid string, controller bool) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetTask",
		Name: name, UID: types.UID("uid-" + uid), Controller: &controller}
}

func TestFleetActionOwnerRefs_OnlyTheControlPlaneSetsOrChangesThem(t *testing.T) {
	v := ownerRefGuarded{&FleetActionValidator{}} // as the webhook serves it
	a := taskRef("dock", "1", true)
	b := taskRef("other", "2", true)
	principals := []struct {
		name string
		ctx  context.Context
		// mayWrite: the principal may add or change an owner reference.
		mayWrite bool
		// mayRemove: the principal may remove a FleetTask's controller reference.
		mayRemove bool
	}{
		{"manager", ctxAsUser(controllerUser), true, true},
		{"user", ctxAsUser("alice"), false, false},
		{"garbage-collector", ctxAsUser(garbageCollectorUser), false, true},
		{"garbage-collector-as-controller-manager", ctxAsUser(kubeControllerManagerUser), false, true},
		{"manager-name-in-another-namespace", ctxAsUser("system:serviceaccount:other:swarmada-manager"), false, false},
		{"garbage-collector-name-in-another-namespace", ctxAsUser("system:serviceaccount:other:generic-garbage-collector"), false, false},
	}
	n := taskRef("dock", "1", false)
	updates := []struct {
		name      string
		old, new  *fleetv1.FleetAction
		isWrite   bool
		isRemoval bool
	}{
		{"add", ownedAction(), ownedAction(a), true, false},
		{"change", ownedAction(a), ownedAction(b), true, false},
		{"add-second", ownedAction(a), ownedAction(a, b), true, false},
		{"remove", ownedAction(a), ownedAction(), false, true},
		{"remove-one-of-two", ownedAction(a, b), ownedAction(b), false, true},
		{"remove-a-non-controller-reference", ownedAction(n), ownedAction(), false, false},
		{"unchanged", ownedAction(a), ownedAction(a), false, false},
	}
	for _, p := range principals {
		for _, u := range updates {
			t.Run(p.name+"/"+u.name, func(t *testing.T) {
				_, err := v.ValidateUpdate(p.ctx, u.old, u.new)
				wantRefused := (u.isWrite && !p.mayWrite) || (u.isRemoval && !p.mayRemove)
				switch {
				case wantRefused && (err == nil || !apierrors.IsForbidden(err)):
					t.Errorf("want Forbidden, got %v", err)
				case !wantRefused && err != nil:
					t.Errorf("want admitted, got %v", err)
				}
			})
		}
	}
}

func TestFleetActionOwnerRefs_CreateControlledByATaskIsTheControlPlanes(t *testing.T) {
	v := ownerRefGuarded{&FleetActionValidator{}} // as the webhook serves it
	cases := []struct {
		name        string
		ctx         context.Context
		action      *fleetv1.FleetAction
		wantRefused bool
	}{
		{"manager-creates-a-task-child", ctxAsUser(controllerUser), ownedAction(taskRef("dock", "1", true)), false},
		{"user-creates-a-task-controlled-action", ctxAsUser("alice"), ownedAction(taskRef("dock", "1", true)), true},
		{"user-creates-with-a-non-controller-task-reference", ctxAsUser("alice"), ownedAction(taskRef("dock", "1", false)), false},
		{"user-creates-controlled-by-a-task-of-another-version", ctxAsUser("alice"), ownedAction(func() metav1.OwnerReference {
			r := taskRef("dock", "1", true)
			r.APIVersion = fleetv1.GroupVersion.Group + "/v2"
			return r
		}()), true},
		{"user-creates-without-owner", ctxAsUser("alice"), ownedAction(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.ValidateCreate(c.ctx, c.action)
			switch {
			case c.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !c.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
		})
	}
}

// A request the webhook cannot attribute is refused, in the closed direction.
func TestFleetActionOwnerRefs_UnattributedWriteRefused(t *testing.T) {
	v := ownerRefGuarded{&FleetActionValidator{}} // as the webhook serves it
	_, err := v.ValidateUpdate(context.Background(), ownedAction(), ownedAction(taskRef("dock", "1", true)))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden for an unattributed owner-reference write, got %v", err)
	}
}
