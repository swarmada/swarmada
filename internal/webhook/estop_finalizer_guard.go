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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/swarmada/swarmada/internal/estopscope"
)

// refuseStopFinalizerRemoval refuses a write that removes the stop finalizer from a carrier
// that carries a stop: its trigger, or the processed marker of a stop whose clear has not been
// applied. The finalizer is held from the moment the trigger lands until the clear has been
// applied (estopscope.StopFinalizer), and removing it sooner ends the stop unapplied, so only
// three writers may: the control plane, which holds and releases it; anyone in a namespace
// being torn down, where the carrier goes with everything else; and a holder of estop-clear on
// the carrier, who may end the stop by hand. A write that keeps the finalizer, or a carrier
// that never held it, is not checked here.
//
// The stored object decides whether the carrier carries a stop, never the incoming one, so a
// write cannot drop the stop's annotations and its finalizer together to pass. reader reads
// the namespace; a nil reader skips the teardown exemption.
func refuseStopFinalizerRemoval(ctx context.Context, authz VerbAuthorizer, reader client.Reader,
	gr schema.GroupResource, oldObj, newObj client.Object) error {
	if !controllerutil.ContainsFinalizer(oldObj, estopscope.StopFinalizer) ||
		controllerutil.ContainsFinalizer(newObj, estopscope.StopFinalizer) {
		return nil
	}
	_, triggered := oldObj.GetAnnotations()[estopTriggeredAnnotation]
	_, processed := oldObj.GetAnnotations()[estopProcessedAnnotation]
	if !triggered && !processed {
		return nil
	}
	if req, err := admission.RequestFromContext(ctx); err == nil && isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	if reader != nil {
		terminating, err := namespaceTerminating(ctx, reader, newObj.GetNamespace())
		if err != nil {
			return err
		}
		if terminating {
			return nil
		}
	}
	err := authorizeEstopVerb(ctx, authz, gr, newObj.GetNamespace(), newObj.GetName(), estopClearVerb)
	if err == nil || !apierrors.IsForbidden(err) {
		return err
	}
	return apierrors.NewForbidden(gr, newObj.GetName(), fmt.Errorf(
		"%s %q in namespace %q carries a stop, and its stop finalizer (%s) is held until the stop's clear "+
			"has been applied. Only the control plane, or a holder of %q on it, removes it before then. "+
			"Clear the stop, and the control plane releases the finalizer once the clear is applied "+
			"(RFC-0001 #safety-estop-scopes). %v",
		gr.Resource, newObj.GetName(), newObj.GetNamespace(), estopscope.StopFinalizer, estopClearVerb, err))
}

// onlyMetadataChangedOn reports whether newObj differs from oldObj in metadata only: every
// other top-level field, spec and status included, is equal. The form of onlyMetadataChanged
// for any kind.
func onlyMetadataChangedOn(oldObj, newObj runtime.Object) bool {
	o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(oldObj)
	if err != nil {
		return false
	}
	n, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newObj)
	if err != nil {
		return false
	}
	delete(o, "metadata")
	delete(n, "metadata")
	return equality.Semantic.DeepEqual(o, n)
}

// controllerMetadataOnly reports whether this write is the control plane's and changes the
// object's metadata only: the stop finalizer it holds or releases, or a marker it records. Such
// a write changes nothing a structural check resolves against, so a carrier that a later change
// elsewhere left inconsistent (an ancestor's tightening) does not refuse the control plane's
// hold of its stop.
func controllerMetadataOnly(ctx context.Context, oldObj, newObj runtime.Object) bool {
	req, err := admission.RequestFromContext(ctx)
	return err == nil && isControllerServiceAccount(req.UserInfo.Username) && onlyMetadataChangedOn(oldObj, newObj)
}
