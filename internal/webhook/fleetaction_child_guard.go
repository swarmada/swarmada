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
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// A FleetTask's child actions are written by the control plane.
//
// A child is a FleetAction that a FleetTask controls through a controller ownerReference.
// The FleetTask controller writes the child's spec from the task's own (RFC-0001 #fleettask:
// the controller is the only writer of the actions it owns), so a change to a child's spec
// from any other identity is refused. So is a change to the child's swarmada.io/ labels and
// annotations, which the control plane keeps as its own record of the child (the dispatch
// sensor finds a child's task by its swarmada.io/fleettask label). Labels and annotations
// outside that prefix stay open. Three writes stay open to a holder of `cancel` on
// fleetactions, the verb that already gates a cancel: a change of spec.desiredState (a pause
// or a resume, as well as the cancel itself), a new cancel request and a new requeue request.
// Each moves the work as a cancel does. A principal without `cancel`, a task source among them
// (RFC-0004), changes the work through the FleetTask instead.
//
// The child is recognised on the stored object, never the requested one, so a write that drops
// the controller reference is still held to this rule; dropping the reference is itself the
// control plane's or the garbage collector's (fleetaction_ownerref_guard.go).

// isFleetTaskControllerRef reports whether ref makes its object a FleetTask's child.
func isFleetTaskControllerRef(ref *metav1.OwnerReference) bool {
	if ref == nil || ref.Controller == nil || !*ref.Controller || ref.Kind != "FleetTask" {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == fleetv1.GroupVersion.Group
}

// swarmadaKeyPrefix is the prefix of the labels and annotations the control plane keeps on a
// child.
const swarmadaKeyPrefix = "swarmada.io/"

// operatorRequestAnnotations are the swarmada.io/ annotations a holder of `cancel` may add to a
// child. Changing or removing one is the control plane's, as for every other swarmada.io/ key.
var operatorRequestAnnotations = []string{cancelRequestedAnnotation, requeueRequestedAnnotation}

// controlPlaneKeysChanged lists, sorted, the swarmada.io/ labels and annotations that differ
// between old and new, except an operator request annotation newly added.
func controlPlaneKeysChanged(oldAction, newAction *fleetv1.FleetAction) []string {
	var out []string
	diff := func(kind string, oldMap, newMap map[string]string, requests []string) {
		for _, m := range []map[string]string{oldMap, newMap} {
			for k := range m {
				if !strings.HasPrefix(k, swarmadaKeyPrefix) || slices.Contains(out, kind+" "+k) {
					continue
				}
				ov, had := oldMap[k]
				nv, has := newMap[k]
				if had == has && ov == nv {
					continue
				}
				if !had && slices.Contains(requests, k) {
					continue
				}
				out = append(out, kind+" "+k)
			}
		}
	}
	diff("label", oldAction.Labels, newAction.Labels, nil)
	diff("annotation", oldAction.Annotations, newAction.Annotations, operatorRequestAnnotations)
	slices.Sort(out)
	return out
}

// guardFleetTaskChild refuses a write to a child's spec or to its swarmada.io/ labels and
// annotations from any principal but the control plane, and a desiredState change, a new cancel
// request or a new requeue request on a child from a principal without `cancel`. An action no
// FleetTask controls is not checked here.
func (v *FleetActionValidator) guardFleetTaskChild(ctx context.Context, oldAction, newAction *fleetv1.FleetAction) error {
	ref := metav1.GetControllerOfNoCopy(oldAction)
	if !isFleetTaskControllerRef(ref) {
		return nil
	}
	oldSpec, newSpec := oldAction.Spec.DeepCopy(), newAction.Spec.DeepCopy()
	oldSpec.DesiredState, newSpec.DesiredState = "", ""
	// The payload is compared on its own: ActionPayload has an unexported field, on which
	// Semantic.DeepEqual panics, and a panic here fails the webhook call, which with
	// failurePolicy Fail refuses every write to the child.
	payloadKept := samePayload(oldSpec.Payload, newSpec.Payload)
	oldSpec.Payload, newSpec.Payload = nil, nil
	specChanged := !payloadKept || !apiequality.Semantic.DeepEqual(oldSpec, newSpec)
	stateChanged := oldAction.Spec.DesiredState != newAction.Spec.DesiredState
	_, hadRequeue := oldAction.Annotations[requeueRequestedAnnotation]
	_, hasRequeue := newAction.Annotations[requeueRequestedAnnotation]
	requeue := executorguard.RequeueChecked(false, hadRequeue, hasRequeue)
	_, hadCancel := oldAction.Annotations[cancelRequestedAnnotation]
	_, hasCancel := newAction.Annotations[cancelRequestedAnnotation]
	cancelRequest := !hadCancel && hasCancel
	keys := controlPlaneKeysChanged(oldAction, newAction)
	if !specChanged && !stateChanged && !requeue && !cancelRequest && len(keys) == 0 {
		return nil
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return apierrors.NewForbidden(fleetTaskGR, newAction.Name,
			fmt.Errorf("cannot authorize a write to a child of FleetTask %q: no admission request in context", ref.Name))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	if specChanged {
		return apierrors.NewForbidden(fleetTaskGR, newAction.Name, fmt.Errorf(
			"user %q may not change the spec of FleetAction %q in namespace %q: it is a child of FleetTask %q, "+
				"and a child's spec is written only by the control plane; change or cancel the FleetTask",
			req.UserInfo.Username, newAction.Name, newAction.Namespace, ref.Name))
	}
	if len(keys) > 0 {
		return apierrors.NewForbidden(fleetTaskGR, newAction.Name, fmt.Errorf(
			"user %q may not add, change or remove %s on FleetAction %q in namespace %q: it is a child of FleetTask %q, "+
				"and a child's swarmada.io/ labels and annotations are written only by the control plane; change or cancel the FleetTask",
			req.UserInfo.Username, strings.Join(keys, ", "), newAction.Name, newAction.Namespace, ref.Name))
	}
	what := "requeue"
	switch {
	case stateChanged:
		what = "change spec.desiredState of"
	case cancelRequest:
		what = "cancel"
	}
	if v.CancelAuthz == nil {
		return apierrors.NewForbidden(fleetTaskGR, newAction.Name, fmt.Errorf(
			"cancel authorization is not configured; refusing to %s FleetAction %q, a child of FleetTask %q",
			what, newAction.Name, ref.Name))
	}
	allowed, err := v.CancelAuthz.Authorize(ctx, req.UserInfo, cancelVerb, fleetTaskGR, newAction.Namespace, newAction.Name)
	if err != nil {
		return apierrors.NewInternalError(fmt.Errorf("authorizing cancel on FleetAction %q: %w", newAction.Name, err))
	}
	if !allowed {
		return apierrors.NewForbidden(fleetTaskGR, newAction.Name, fmt.Errorf(
			"user %q may not %s FleetAction %q in namespace %q: it is a child of FleetTask %q, and pausing, "+
				"resuming, cancelling or requeueing a child needs the cancel verb on fleetactions; pause or cancel the FleetTask",
			req.UserInfo.Username, what, newAction.Name, newAction.Namespace, ref.Name))
	}
	return nil
}

// samePayload reports whether a write left a child's payload as it was: absent on both sides, or
// present on both in the same JSON form. The API server encodes the old and the new object alike,
// so an apply that spells the stored value another way arrives byte for byte the same. The form
// counts as well as the bytes: {"raw": <base64>} and the object it decodes to deliver the same
// bytes to an adapter, and they are two specs.
func samePayload(oldPayload, newPayload *fleetv1.ActionPayload) bool {
	if oldPayload == nil || newPayload == nil {
		return oldPayload == newPayload
	}
	// MarshalJSON cannot fail: it returns the bytes it holds or encodes a []byte, and
	// TestActionPayload_MarshalJSONCannotFail holds it to that.
	oldJSON, _ := oldPayload.MarshalJSON()
	newJSON, _ := newPayload.MarshalJSON()
	return bytes.Equal(oldJSON, newJSON)
}
