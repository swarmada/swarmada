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
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Owner references on a FleetAction are the control plane's to write.
//
// A FleetTask's children are the FleetActions it controls through a controller
// ownerReference: that reference is what the FleetTask controller acts on when it fans the
// task's desired state out to its members, and what the garbage collector follows when the
// task is deleted (RFC-0001 #fleettask: the controller is the only writer of the actions it
// owns). Only the control plane creates those children, so only the control plane sets or
// changes an owner reference on a FleetAction.
//
// Removing a FleetTask's controller reference is the control plane's or the garbage
// collector's, which removes it when it orphans a deleted task's children. Removed by anyone
// else, it would make a child an ordinary action that the child rule no longer holds
// (fleetaction_child_guard.go). Removing any other owner reference makes no object anyone's
// child, and stays open to everyone.

// ownerRefGuarded is the FleetAction validator as the webhook serves it: the owner-reference
// guard, and on an update the child guard, run ahead of the validator's own checks.
type ownerRefGuarded struct{ *FleetActionValidator }

var _ webhook.CustomValidator = ownerRefGuarded{}

// ValidateCreate refuses a FleetAction created controlled by a FleetTask, except from the
// control plane, then applies FleetActionValidator.ValidateCreate.
func (g ownerRefGuarded) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	if action, ok := obj.(*fleetv1.FleetAction); ok {
		if err := guardFleetActionOwnerRefsOnCreate(ctx, action); err != nil {
			return nil, err
		}
	}
	return g.FleetActionValidator.ValidateCreate(ctx, obj)
}

// ValidateUpdate refuses an update that adds or changes an owner reference, except from the
// control plane, or that removes a FleetTask's controller reference, except from the control
// plane or the garbage collector; then holds a FleetTask's child to the child rule; then
// applies FleetActionValidator.ValidateUpdate.
func (g ownerRefGuarded) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldAction, okOld := oldObj.(*fleetv1.FleetAction)
	newAction, okNew := newObj.(*fleetv1.FleetAction)
	if okOld && okNew {
		if err := guardFleetActionOwnerRefsOnUpdate(ctx, oldAction, newAction); err != nil {
			return nil, err
		}
		if err := g.guardFleetTaskChild(ctx, oldAction, newAction); err != nil {
			return nil, err
		}
	}
	return g.FleetActionValidator.ValidateUpdate(ctx, oldObj, newObj)
}

// guardFleetActionOwnerRefsOnCreate refuses a FleetAction created with a FleetTask controller
// reference by any principal but the control plane.
func guardFleetActionOwnerRefsOnCreate(ctx context.Context, action *fleetv1.FleetAction) error {
	if !isFleetTaskControllerRef(metav1.GetControllerOfNoCopy(action)) {
		return nil
	}
	return refuseOwnerRefWrite(ctx, action, "create a FleetAction controlled by a FleetTask", false)
}

// guardFleetActionOwnerRefsOnUpdate refuses an update that adds or changes an owner reference
// on a FleetAction, from any principal but the control plane, and one that removes a FleetTask's
// controller reference, from any principal but the control plane or the garbage collector. An
// update that removes another owner reference, or leaves them as they are, is admitted.
func guardFleetActionOwnerRefsOnUpdate(ctx context.Context, oldAction, newAction *fleetv1.FleetAction) error {
	for _, ref := range newAction.OwnerReferences {
		if !containsOwnerRef(oldAction.OwnerReferences, ref) {
			return refuseOwnerRefWrite(ctx, newAction, "add or change an owner reference of a FleetAction", false)
		}
	}
	for i := range oldAction.OwnerReferences {
		ref := &oldAction.OwnerReferences[i]
		if isFleetTaskControllerRef(ref) && !containsOwnerRef(newAction.OwnerReferences, *ref) {
			return refuseOwnerRefWrite(ctx, newAction, "remove the FleetTask controller reference of a FleetAction", true)
		}
	}
	return nil
}

func containsOwnerRef(refs []metav1.OwnerReference, ref metav1.OwnerReference) bool {
	for _, r := range refs {
		if apiequality.Semantic.DeepEqual(r, ref) {
			return true
		}
	}
	return false
}

// refuseOwnerRefWrite admits the control plane's own principal, and the garbage collector when
// gc is set, and refuses everyone else, including a request it cannot attribute.
func refuseOwnerRefWrite(ctx context.Context, action *fleetv1.FleetAction, what string, gc bool) error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskGR, action.Name,
			fmt.Errorf("cannot authorize the owner-reference write: no admission request in context"))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	writers := "the control plane"
	if gc {
		if isGarbageCollector(req.UserInfo.Username) {
			return nil
		}
		writers = "the control plane or the garbage collector"
	}
	return apierrors.NewForbidden(fleetTaskGR, action.Name,
		fmt.Errorf("user %q may not %s: this write is made only by %s", req.UserInfo.Username, what, writers))
}
