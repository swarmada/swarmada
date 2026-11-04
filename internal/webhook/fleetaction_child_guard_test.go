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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A FleetTask's child is written by the control plane: a change to its spec or to its
// swarmada.io/ labels and annotations from anyone else is refused, and a desiredState change,
// a new cancel request or a new requeue request needs `cancel`.

func childAction(refs ...metav1.OwnerReference) *fleetv1.FleetAction {
	a := ownedAction(refs...)
	a.Spec.Priority = fleetv1.ActionPriorityNormal
	a.Spec.DesiredState = fleetv1.DesiredStateRunning
	return a
}

func withPriority(a *fleetv1.FleetAction, p fleetv1.ActionPriority) *fleetv1.FleetAction {
	a.Spec.Priority = p
	return a
}

func withDesiredState(a *fleetv1.FleetAction, s fleetv1.DesiredState) *fleetv1.FleetAction {
	a.Spec.DesiredState = s
	return a
}

func withRequeue(a *fleetv1.FleetAction) *fleetv1.FleetAction {
	a.Annotations = map[string]string{requeueRequestedAnnotation: "upstream"}
	return a
}

func withLabel(a *fleetv1.FleetAction) *fleetv1.FleetAction {
	a.Labels = map[string]string{"team": "east"}
	return a
}

// withLabels sets the labels to kv, read as key, value pairs.
func withLabels(a *fleetv1.FleetAction, kv ...string) *fleetv1.FleetAction {
	a.Labels = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		a.Labels[kv[i]] = kv[i+1]
	}
	return a
}

// withAnnotations sets the annotations to kv, read as key, value pairs.
func withAnnotations(a *fleetv1.FleetAction, kv ...string) *fleetv1.FleetAction {
	a.Annotations = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		a.Annotations[kv[i]] = kv[i+1]
	}
	return a
}

func TestFleetActionChild_SpecIsTheControlPlanes(t *testing.T) {
	task := taskRef("dock", "1", true)
	yes := true
	other := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "dock", UID: "uid-9", Controller: &yes}
	cases := []struct {
		name        string
		user        string
		cancel      bool
		old, new    *fleetv1.FleetAction
		wantRefused bool
	}{
		{"user-changes-a-childs-priority", "alice", true, childAction(task), withPriority(childAction(task), fleetv1.ActionPriorityCritical), true},
		{"manager-changes-a-childs-priority", controllerUser, false, childAction(task), withPriority(childAction(task), fleetv1.ActionPriorityCritical), false},
		{"user-without-cancel-pauses-a-child", "alice", false, childAction(task), withDesiredState(childAction(task), fleetv1.DesiredStatePaused), true},
		{"user-with-cancel-pauses-a-child", "alice", true, childAction(task), withDesiredState(childAction(task), fleetv1.DesiredStatePaused), false},
		{"user-without-cancel-resumes-a-child", "alice", false, withDesiredState(childAction(task), fleetv1.DesiredStatePaused), childAction(task), true},
		{"user-with-cancel-resumes-a-child", "alice", true, withDesiredState(childAction(task), fleetv1.DesiredStatePaused), childAction(task), false},
		{"user-without-cancel-requeues-a-child", "alice", false, childAction(task), withRequeue(childAction(task)), true},
		{"user-with-cancel-requeues-a-child", "alice", true, childAction(task), withRequeue(childAction(task)), false},
		{"manager-pauses-a-child", controllerUser, false, childAction(task), withDesiredState(childAction(task), fleetv1.DesiredStatePaused), false},
		{"user-labels-a-child", "alice", false, childAction(task), withLabel(childAction(task)), false},
		{"user-changes-an-action-with-no-owner", "alice", false, childAction(), withPriority(childAction(), fleetv1.ActionPriorityCritical), false},
		{"user-pauses-an-action-with-no-owner", "alice", false, childAction(), withDesiredState(childAction(), fleetv1.DesiredStatePaused), false},
		{"user-changes-an-action-another-kind-controls", "alice", false, childAction(other), withPriority(childAction(other), fleetv1.ActionPriorityCritical), false},
		{"user-changes-an-action-a-task-refers-to-without-controlling", "alice", false,
			childAction(taskRef("dock", "1", false)), withPriority(childAction(taskRef("dock", "1", false)), fleetv1.ActionPriorityCritical), false},
		// The child is recognised on the stored object: the requested one dropping the
		// reference does not take the write out of the rule.
		{"user-drops-the-reference-and-edits-in-one-write", "alice", true, childAction(task), withPriority(childAction(), fleetv1.ActionPriorityCritical), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			authz := &fakeEstopAuthz{allow: c.cancel}
			v := &FleetActionValidator{CancelAuthz: authz}
			err := v.guardFleetTaskChild(ctxAsUser(c.user), c.old, c.new)
			switch {
			case c.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !c.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
			if authz.calls > 0 && authz.gotVerb != cancelVerb {
				t.Errorf("authorized verb %q, want %q", authz.gotVerb, cancelVerb)
			}
		})
	}
}

func TestFleetActionChild_SwarmadaMetadataIsTheControlPlanes(t *testing.T) {
	task := taskRef("dock", "1", true)
	const taskLabel = "swarmada.io/fleettask"
	// child is a child as the controller writes it, with its task's label.
	child := func(refs ...metav1.OwnerReference) *fleetv1.FleetAction {
		return withLabels(childAction(refs...), taskLabel, "dock")
	}
	cases := []struct {
		name        string
		user        string
		cancel      bool
		old, new    *fleetv1.FleetAction
		wantRefused bool
	}{
		{"user-changes-a-childs-task-label", "alice", true, child(task), withLabels(child(task), taskLabel, "other"), true},
		{"user-removes-a-childs-task-label", "alice", true, child(task), withLabels(child(task)), true},
		{"user-adds-a-swarmada-label-to-a-child", "alice", true, child(task),
			withLabels(child(task), taskLabel, "dock", "swarmada.io/fleettask-member", "a"), true},
		{"user-adds-a-swarmada-annotation-to-a-child", "alice", true, child(task),
			withAnnotations(child(task), "swarmada.io/failure-alerted", "true"), true},
		{"user-removes-a-swarmada-annotation-from-a-child", "alice", true,
			withAnnotations(child(task), "swarmada.io/failure-alerted", "true"), child(task), true},
		{"user-adds-a-label-named-like-a-request", "alice", true, child(task),
			withLabels(child(task), taskLabel, "dock", requeueRequestedAnnotation, "upstream"), true},
		{"user-with-cancel-requests-a-cancel-of-a-child", "alice", true, child(task),
			withAnnotations(child(task), cancelRequestedAnnotation, "upstream"), false},
		{"user-without-cancel-requests-a-cancel-of-a-child", "alice", false, child(task),
			withAnnotations(child(task), cancelRequestedAnnotation, "upstream"), true},
		{"user-with-cancel-changes-a-requeue-request", "alice", true, withRequeue(child(task)),
			withAnnotations(child(task), requeueRequestedAnnotation, "another reason"), true},
		{"user-with-cancel-withdraws-a-requeue-request", "alice", true, withRequeue(child(task)), child(task), true},
		{"user-with-cancel-withdraws-a-cancel-request", "alice", true,
			withAnnotations(child(task), cancelRequestedAnnotation, "upstream"), child(task), true},
		{"manager-relabels-a-child", controllerUser, false, child(task), withLabels(child(task), taskLabel, "other"), false},
		{"manager-withdraws-a-requeue-request", controllerUser, false, withRequeue(child(task)), child(task), false},
		{"user-labels-a-child-outside-the-prefix", "alice", false, child(task),
			withLabels(child(task), taskLabel, "dock", "team", "east"), false},
		{"user-annotates-a-child-outside-the-prefix", "alice", false, child(task),
			withAnnotations(child(task), "example.com/note", "aisle 4"), false},
		{"user-relabels-an-action-with-no-owner", "alice", false, child(), withLabels(child(), taskLabel, "other"), false},
		// The child is recognised on the stored object, as for its spec.
		{"user-drops-the-reference-and-relabels-in-one-write", "alice", true, child(task), withLabels(child(), taskLabel, "other"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			authz := &fakeEstopAuthz{allow: c.cancel}
			v := &FleetActionValidator{CancelAuthz: authz}
			err := v.guardFleetTaskChild(ctxAsUser(c.user), c.old, c.new)
			switch {
			case c.wantRefused && (err == nil || !apierrors.IsForbidden(err)):
				t.Errorf("want Forbidden, got %v", err)
			case !c.wantRefused && err != nil:
				t.Errorf("want admitted, got %v", err)
			}
			if authz.calls > 0 && authz.gotVerb != cancelVerb {
				t.Errorf("authorized verb %q, want %q", authz.gotVerb, cancelVerb)
			}
		})
	}
}

// The guard runs where the webhook serves the validator: a refused write never reaches it.
func TestFleetActionChild_GuardIsServed(t *testing.T) {
	task := taskRef("dock", "1", true)
	v := ownerRefGuarded{&FleetActionValidator{CancelAuthz: &fakeEstopAuthz{allow: true}}}
	if _, err := v.ValidateUpdate(ctxAsUser("alice"), childAction(task), withPriority(childAction(task), fleetv1.ActionPriorityCritical)); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden for a change to a child's spec through the served validator, got %v", err)
	}
}

// A write the guard cannot attribute or authorize is refused, in the closed direction.
func TestFleetActionChild_FailsClosed(t *testing.T) {
	task := taskRef("dock", "1", true)
	paused := func() *fleetv1.FleetAction { return withDesiredState(childAction(task), fleetv1.DesiredStatePaused) }
	t.Run("unattributed", func(t *testing.T) {
		v := &FleetActionValidator{CancelAuthz: &fakeEstopAuthz{allow: true}}
		if err := v.guardFleetTaskChild(context.Background(), childAction(task), paused()); err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("want Forbidden, got %v", err)
		}
	})
	t.Run("no-authorizer", func(t *testing.T) {
		v := &FleetActionValidator{}
		if err := v.guardFleetTaskChild(ctxAsUser("alice"), childAction(task), paused()); err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("want Forbidden, got %v", err)
		}
	})
	t.Run("unattributed-relabel", func(t *testing.T) {
		v := &FleetActionValidator{CancelAuthz: &fakeEstopAuthz{allow: true}}
		relabelled := withLabels(childAction(task), "swarmada.io/fleettask", "other")
		if err := v.guardFleetTaskChild(context.Background(), childAction(task), relabelled); err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("want Forbidden, got %v", err)
		}
	})
	t.Run("no-authorizer-cancel-request", func(t *testing.T) {
		v := &FleetActionValidator{}
		requested := withAnnotations(childAction(task), cancelRequestedAnnotation, "upstream")
		if err := v.guardFleetTaskChild(ctxAsUser("alice"), childAction(task), requested); err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("want Forbidden, got %v", err)
		}
	})
	t.Run("authorizer-error", func(t *testing.T) {
		v := &FleetActionValidator{CancelAuthz: &fakeEstopAuthz{err: errors.New("SubjectAccessReview unavailable")}}
		err := v.guardFleetTaskChild(ctxAsUser("alice"), childAction(task), paused())
		if err == nil || !apierrors.IsInternalError(err) {
			t.Errorf("want an internal error, got %v", err)
		}
	})
}
