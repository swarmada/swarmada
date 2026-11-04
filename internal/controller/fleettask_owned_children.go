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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A FleetTask acts only on the FleetActions it created (RFC-0001 #fleettask: the controller is
// the only writer of the actions it owns).
//
// The deterministic child names (childName, iterationChildName, compChildName) find a child;
// they do not make one. An object is the task's child when the task controls it through its
// controller ownerReference. An object of that name the task does not control is a name
// conflict: no fan-out, prune, compensation or status read touches it, its member is held as a
// refused create is held, and the task says so (ChildrenAdmitted False, reason NameConflict, and
// one Warning Event). The member starts once the name is free.

// EventChildNameConflict is the Warning Event emitted, once per conflict, when a FleetAction
// the task does not control already has the name a member's child would get.
const EventChildNameConflict = "ChildNameConflict"

// reasonChildNameConflict is ConditionChildrenAdmitted's reason for a name conflict.
const reasonChildNameConflict = "NameConflict"

// ownedChild reports whether obj is a child of task: an object the task controls through its
// controller ownerReference, compared by UID. A child's name and its ownedByLabel do not make it
// one, because any principal that writes FleetActions can give an object either.
func ownedChild(obj metav1.Object, task *fleetv1.FleetTask) bool {
	return metav1.IsControlledBy(obj, task)
}

// childNameConflictMessage is the ChildrenAdmitted message for a name conflict: it names the
// member and the existing object, and says the task leaves that object unchanged.
func childNameConflictMessage(member, name string) string {
	return fmt.Sprintf("a FleetAction named %q already exists and this task does not own it, so action %q cannot start; "+
		"the task leaves that FleetAction unchanged. Delete or rename it to let the action start", name, member)
}

func childNameConflictError(kind, member, name string) error {
	return fmt.Errorf("%s %q of action %q: a FleetAction of that name exists and is not owned by this task", kind, name, member)
}

// recordChildNameConflict is recordChildRefused for a member whose child name is taken by a
// FleetAction the task does not control: the condition and the Event are written when the
// condition changes (RA-1), under the resourceVersion the reconcile read, and nothing is written
// to that FleetAction. The error it returns retries the pass on the manager's backoff, so the
// member starts once the name is free.
func (r *FleetTaskReconciler) recordChildNameConflict(ctx context.Context, original *fleetv1.FleetTask, member, name string) error {
	conflict := childNameConflictError("child", member, name)
	msg := childNameConflictMessage(member, name)
	if prev := apimeta.FindStatusCondition(original.Status.Conditions, ConditionChildrenAdmitted); prev != nil &&
		prev.Status == metav1.ConditionFalse && prev.Message == msg {
		return conflict
	}
	held := original.DeepCopy()
	setCond(held, ConditionChildrenAdmitted, metav1.ConditionFalse, reasonChildNameConflict, msg)
	held.Status.ObservedGeneration = held.Generation
	if err := r.Status().Patch(ctx, held, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("%w (recording the conflict on the task: %v)", conflict, err)
	}
	if r.Recorder != nil {
		r.Recorder.Event(held, "Warning", EventChildNameConflict, msg)
	}
	return conflict
}

// compensationConflict names a member whose compensation child name is taken by a FleetAction
// the task does not control.
type compensationConflict struct{ member, name string }

// holdCompensationConflict reports a compensation name conflict on the in-memory status, which
// the pass's own status patch writes: ChildrenAdmitted False, and one Warning Event when the
// condition changes. A condition already reporting the same conflict is kept as stored, so a
// pass that changes nothing writes nothing (RA-1). It returns an error so the pass is retried
// on the manager's backoff and the compensation starts once the name is free.
func (r *FleetTaskReconciler) holdCompensationConflict(task, original *fleetv1.FleetTask, member, name string) error {
	msg := childNameConflictMessage(member, name)
	if prev := apimeta.FindStatusCondition(original.Status.Conditions, ConditionChildrenAdmitted); prev != nil &&
		prev.Status == metav1.ConditionFalse && prev.Message == msg {
		apimeta.SetStatusCondition(&task.Status.Conditions, *prev)
	} else {
		setCond(task, ConditionChildrenAdmitted, metav1.ConditionFalse, reasonChildNameConflict, msg)
		if r.Recorder != nil {
			r.Recorder.Event(task, "Warning", EventChildNameConflict, msg)
		}
	}
	return childNameConflictError("compensation child", member, name)
}

// flowNameConflicts collects, for one pass of a flow task, the members whose child name is
// taken by a FleetAction the task does not control.
type flowNameConflicts struct {
	task        *fleetv1.FleetTask
	heldMembers map[string]bool
	first       *compensationConflict
}

func newFlowNameConflicts(task *fleetv1.FleetTask) *flowNameConflicts {
	return &flowNameConflicts{task: task, heldMembers: map[string]bool{}}
}

func flowConflictKey(member string, iteration int32) string {
	return fmt.Sprintf("%s/%d", member, iteration)
}

// owned passes a child lookup's error through, except when the lookup found an object the task
// does not control: that is recorded as a conflict and returned as NotFound, so the evaluator
// sees no child there and nothing reads or writes the object.
func (c *flowNameConflicts) owned(err error, child *fleetv1.FleetAction, member string, iteration int32) error {
	if err != nil || ownedChild(child, c.task) {
		return err
	}
	c.heldMembers[flowConflictKey(member, iteration)] = true
	if c.first == nil {
		c.first = &compensationConflict{member: member, name: child.Name}
	}
	return apierrors.NewNotFound(fleetv1.GroupVersion.WithResource("fleetactions").GroupResource(), child.Name)
}

// held reports whether this member's child name is taken; its child is not created.
func (c *flowNameConflicts) held(member string, iteration int32) bool {
	return c.heldMembers[flowConflictKey(member, iteration)]
}

func (c *flowNameConflicts) none() bool { return c.first == nil }

// report returns a refused create's error when there is one, as before; otherwise it records
// the first name conflict on the task.
func (c *flowNameConflicts) report(ctx context.Context, r *FleetTaskReconciler, original *fleetv1.FleetTask, refused func() error) error {
	if refused != nil {
		return refused()
	}
	return r.recordChildNameConflict(ctx, original, c.first.member, c.first.name)
}
