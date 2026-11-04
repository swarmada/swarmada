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
)

// The garbage collector deletes the members of a FleetTask that was deleted. It is admitted
// under each username it runs as, and no other principal is admitted by a similar name. The
// guard here has no reader, so a delete it does not admit outright is refused unchecked.
func TestFleetActionDelete_GarbageCollectorIsAdmitted(t *testing.T) {
	g := &FleetActionDeleteGuard{}
	cases := []struct {
		name        string
		ctx         context.Context
		wantRefused bool
	}{
		{"manager", ctxAsUser(controllerUser), false},
		{"garbage-collector", ctxAsUser(garbageCollectorUser), false},
		{"garbage-collector-as-controller-manager", ctxAsUser(kubeControllerManagerUser), false},
		{"user", ctxAsUser("alice"), true},
		{"garbage-collector-name-in-another-namespace", ctxAsUser("system:serviceaccount:other:generic-garbage-collector"), true},
		{"unattributed", context.Background(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := g.ValidateDelete(c.ctx, ownedAction(taskRef("dock", "1", true)))
			switch {
			case c.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !c.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
		})
	}
}

// The delete guard and the owner-reference guard let the same principals through as the
// garbage collector: whoever may delete a deleted task's member may also orphan it, and no one
// else may do either.
func TestFleetActionGuards_AgreeOnTheGarbageCollector(t *testing.T) {
	del := &FleetActionDeleteGuard{}
	refs := ownerRefGuarded{&FleetActionValidator{}}
	for _, user := range []string{
		garbageCollectorUser,
		kubeControllerManagerUser,
		"alice",
		"system:kube-scheduler",
		"system:serviceaccount:kube-system:replicaset-controller",
		"system:serviceaccount:other:generic-garbage-collector",
	} {
		t.Run(user, func(t *testing.T) {
			child := ownedAction(taskRef("dock", "1", true))
			_, delErr := del.ValidateDelete(ctxAsUser(user), child)
			_, rmErr := refs.ValidateUpdate(ctxAsUser(user), child, ownedAction())
			if (delErr == nil) != (rmErr == nil) {
				t.Errorf("the guards disagree: delete %v; removal of the task's controller reference %v", delErr, rmErr)
			}
		})
	}
}
