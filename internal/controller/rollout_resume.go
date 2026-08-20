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
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Operator resume for a Paused rollout (ADR-0041), shared by both rollout kinds.
//
// WHY THIS EXISTS. strategy.rollingUpdate.pauseOnError defaults to TRUE, and a paused
// rollout previously had no exit: no resume path existed, and the delete webhook admits
// only terminal (Succeeded/Failed) records — so a single failed robot wedged the rollout
// permanently, and for a ModelRollout that also stranded every capability the in-flight
// models grant, because the rollout's own status is what un-suspends them. The only escape
// was to repair every failed robot out of band until the failure set emptied, because
// `paused` is derived per reconcile rather than latched.
//
// SHAPE. Operator intake is an annotation consumed by the controller, gated in the CLI by a
// SelfSubjectAccessReview on the rollout-resume custom verb — the same split ModelPolicy's
// policy-reset uses (§9.1.9.4): status stays control-plane-owned, the operator's intent is a
// spec-side fact, and RBAC plus the audit trail apply at one enforcement point.
//
// SEMANTICS. Resume EXCLUDES the failed robots from further attempts by this rollout; it does
// NOT retry them. A fresh rollout is the retry path. This mirrors Auto model rollback, whose
// RolledBackRobots are likewise "excluded from further update attempts by this rollout (a fixed
// model needs a fresh rollout), so a reverted robot is never pushed back into an update loop".
// Retrying in place would re-dispatch the artifact that just failed and re-pause on the same
// robots — a loop, not a recovery. See ADR-0041.
const (
	// rolloutResumeAnnotation carries the operator's reason. A NEW value is what makes the
	// controller act, so a second resume after a second pause fires again rather than being
	// deduplicated away.
	rolloutResumeAnnotation = "swarmada.io/rollout-resume"
	// rolloutResumeProcessedAnnotation records the value already applied, making a
	// re-reconcile of the same request free (RA-1: no write per reconcile).
	rolloutResumeProcessedAnnotation = "swarmada.io/rollout-resume-processed"
)

// pendingResume returns the resume reason awaiting application, and whether there is one.
// A request whose value already appears in the processed marker is not pending.
func pendingResume(annotations map[string]string) (string, bool) {
	req, requested := annotations[rolloutResumeAnnotation]
	if !requested {
		return "", false
	}
	if annotations[rolloutResumeProcessedAnnotation] == req {
		return "", false
	}
	return req, true
}

// mergeExcluded folds newly-excluded robot names into the existing excluded set, returning a
// sorted, deduplicated list. Sorted because the result is compared with DeepEqual to decide
// whether to write status: an order that varied with map iteration would write every reconcile.
func mergeExcluded(existing, add []string) []string {
	set := make(map[string]bool, len(existing)+len(add))
	for _, n := range existing {
		set[n] = true
	}
	for _, n := range add {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// markResumeProcessed stamps the processed marker so the same request cannot fire twice.
// Called whether or not the rollout was actually Paused: a stale annotation left on a running
// rollout must not silently resume a FUTURE pause.
func markResumeProcessed(ctx context.Context, c client.Client, obj client.Object, req string) error {
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[rolloutResumeProcessedAnnotation] = req
	obj.SetAnnotations(ann)
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording rollout-resume processed marker: %w", err)
	}
	return nil
}
