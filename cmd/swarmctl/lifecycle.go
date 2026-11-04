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

package main

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Well-known annotation keys the control-plane controllers watch as the intake
// for the custom verbs. swarmctl writes these ONLY after the matching
// SelfSubjectAccessReview gate passes (see cli.RequireVerb), so the RBAC verb —
// not a bare metadata patch — is what authorizes the action.
//
// These mirror the controller-side constants verbatim; they are the documented
// wire between the CLI verb and the reconciler:
//   - internal/controller/fleetaction_controller.go  (annCancelRequested)
//   - internal/controller/zoneestop_controller.go  (annEstopTriggered)
const (
	annCancelRequested = "swarmada.io/cancel-requested"
	annEstopTriggered  = "swarmada.io/estop-triggered"
)

// THE ORDER EVERY MUTATING COMMAND FOLLOWS: fetch, authorise, prompt, write.
//
// Before this was written down there were three orders in one package. `cancel`,
// `estop trigger`, `estop-clear`, `admit` and `policy reset` gated first; `delete
// robot` fetched first to decide which kind it was deleting. The visible cost was
// on `cancel`: an operator who mistyped a name, or named a FleetTask, was told
// they lacked permission — for an object that was not there — and was shown a
// destructive confirmation prompt for it first.
//
// Writing the rule down (2026-09-08) did not make the package follow it. Only the
// two `cancel` paths were reordered then, and for six days this comment described
// an order that four commands did not use: `estop trigger` and `estop-clear` still
// put an EMERGENCY-STOP confirmation in front of a zone they had not read, and
// `admit`, `reject`, `policy reset` and `rollout resume` still gated first — two of
// them arguing in their own comments for the posture this one rejects. Every
// mutating command follows it as of 2026-09-14, and TestVerbGateOrderIsUniform now
// has a case for each, so the next divergence fails a test instead of ageing here.
//
//  1. FETCH. Resolve the object before saying anything about it. A not-found or
//     wrong-kind condition is then reported as what it is.
//  2. AUTHORISE. cli.RequireVerb, fail closed, before any write.
//  3. PROMPT. cli.Confirm names an object that has been verified to exist and
//     that the caller is permitted to act on, so answering "yes" cannot be wasted.
//  4. WRITE.
//
// WHY FETCHING FIRST DOES NOT WEAKEN THE GATE. The fetch is an ordinary Kubernetes
// `get`, enforced server-side against the caller's read RBAC: a caller who may not
// read the object receives Forbidden from the fetch and learns nothing. The custom
// verb (fleetactions/cancel, discoveredrobots/admit, …) is an ADDITIONAL grant on
// top of read, so a caller who reaches step 2 could already have run `swarmctl get`
// on the same object. Nothing is disclosed that read access did not already
// disclose, and no write happens before the verb check.
//
// The one deliberate exception is `deleteAdmittedRobot`, which has no custom-verb
// gate at all because the standard `delete` verb is enforced server-side on the
// Delete call itself; it still fetches before prompting, so steps 1, 3 and 4 hold.
// `robotClassRollout` is the same shape: no custom verb, fetch then prompt.
//
// The two estop paths add a step: they re-read the zone between the prompt and the
// write. A confirmation can be held open for as long as an operator takes to answer
// it, and on a safety path the write must be computed from the zone as it is then —
// `removeAnnotation` in particular is a no-op on a copy whose trigger annotation was
// absent when it was read, so clearing from a pre-prompt copy could report a zone
// resumed while a trigger raised during the prompt still stood.

// stripKind lets a command accept an optional leading kind token, so both
// `swarmctl admit dr-foo` and the RFC-0001 spelling `swarmctl admit
// discoveredrobot dr-foo` work. If args[0] resolves to want, it is dropped.
func stripKind(args []string, want *resourceDef) []string {
	if len(args) > 0 {
		if def, err := resolveResource(args[0]); err == nil && def == want {
			return args[1:]
		}
	}
	return args
}

// patchAnnotation sets key=val in obj's annotations via a merge patch against the
// freshly-fetched object. It is the delivery step of a custom verb, never its
// authorization — callers gate with cli.RequireVerb first.
func patchAnnotation(ctx context.Context, c client.Client, obj client.Object, key, val string) error {
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = val
	obj.SetAnnotations(ann)
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("writing %s annotation: %w", key, err)
	}
	return nil
}

// removeAnnotation deletes key from obj's annotations via a merge patch. A merge
// patch cannot delete a map key, so this uses a JSON merge patch that nulls it.
func removeAnnotation(ctx context.Context, c client.Client, obj client.Object, key string) error {
	if _, present := obj.GetAnnotations()[key]; !present {
		return nil
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	delete(ann, key)
	obj.SetAnnotations(ann)
	// client.MergeFrom produces an RFC-7386 JSON merge patch, which represents a
	// removed key as an explicit null — so the deletion is applied server-side.
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("clearing %s annotation: %w", key, err)
	}
	return nil
}
