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
