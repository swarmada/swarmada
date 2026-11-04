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
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/swarmada/swarmada/internal/estopscope"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// An emergency stop is never refused for something unrelated to the stop (RFC-0001
// #safety-emergency-stop-protocol). Each estop carrier's validating webhook re-checks
// invariants on an update, and some of them resolve against other objects or against
// rules that can change after the carrier was admitted. The estop and its clear are
// updates too, so each carrier exempts the one write that changes nothing but the estop
// annotations. The exemption is defined once, here, so it is the same on the Robot, the
// FleetZone and the SwarmadaConfig.

// estopAnnotationKeys are the annotations an estop, a clear or a resume request writes
// on any carrier: the trigger the operator sets, the actor the mutating stamper adds to
// the same request, and the idempotence marker the controller records; and, on a Robot,
// the operator's resume request, the marker recording it was handled, and the claim the
// controller holds on it until it is answered. The resume
// keys are listed so that neither the request nor the controller's marker write is
// refused by an invariant about something else: a marker that could not be written
// would leave the request pending, to be evaluated again later.
var estopAnnotationKeys = []string{
	estopTriggeredAnnotation, AnnEstopActor, estopProcessedAnnotation,
	fleetv1.EstopResumeAnnotation, fleetv1.EstopResumeProcessedAnnotation, fleetv1.EstopResumeClaimedAnnotation,
}

// onlyEstopAnnotationsChangedOn reports whether newObj differs from oldObj in the estop
// annotations and in nothing else a writer controls. Server-managed metadata
// (resourceVersion, generation, managedFields) and status, which the validating hooks
// never govern, are left out of the comparison, and so are the annotations named in
// ignored. A write that also changes a label, another annotation or any spec field is
// not exempt: its other change is validated as usual.
//
// clearStatus zeroes obj's status; it is per kind because status has no common accessor.
func onlyEstopAnnotationsChangedOn[T client.Object](oldObj, newObj T, ignored []string, clearStatus func(T)) bool {
	estopChanged := false
	for _, k := range estopAnnotationKeys {
		ov, oh := oldObj.GetAnnotations()[k]
		nv, nh := newObj.GetAnnotations()[k]
		if oh != nh || ov != nv {
			estopChanged = true
		}
	}
	if !estopChanged {
		return false
	}
	o := withoutEstopAnnotations(oldObj, ignored, clearStatus)
	n := withoutEstopAnnotations(newObj, ignored, clearStatus)
	return equality.Semantic.DeepEqual(o, n)
}

// withoutEstopAnnotations is a copy of obj with the estop annotations, the ignored
// annotations, server-managed metadata and status removed, for
// onlyEstopAnnotationsChangedOn.
func withoutEstopAnnotations[T client.Object](obj T, ignored []string, clearStatus func(T)) T {
	c := obj.DeepCopyObject().(T)
	ann := c.GetAnnotations()
	for _, k := range estopAnnotationKeys {
		delete(ann, k)
	}
	for _, k := range ignored {
		delete(ann, k)
	}
	if len(ann) == 0 {
		ann = nil
	}
	c.SetAnnotations(ann)
	// The stop finalizer rides on the trigger write (the estop-actor defaulter holds it on the
	// carrier the moment the trigger lands) and leaves with the clear: it is part of the estop
	// write, not a change of the object.
	var fin []string
	for _, f := range c.GetFinalizers() {
		if f != estopscope.StopFinalizer {
			fin = append(fin, f)
		}
	}
	c.SetFinalizers(fin)
	c.SetResourceVersion("")
	c.SetGeneration(0)
	c.SetManagedFields(nil)
	clearStatus(c)
	return c
}
