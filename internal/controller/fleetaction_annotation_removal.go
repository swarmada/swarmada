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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// removeActionAnnotation removes key from action, which is the action as the pass last read or
// wrote it. The patch is locked to that version. A merge patch replaces the annotation map when
// the removal leaves it empty, so without the lock an annotation written since the read would be
// erased with it.
//
// On a conflict the action is read again from the API server and key is removed from that copy,
// whatever its value now is. A requeue request written over a standing one is removed with it:
// the requeue check admits a request only when the annotation is added, so a value written over a
// standing request was never checked, and acting on it could move work a stop holds. The action
// is left as last written or read. After removalAttempts conflicts in a row the conflict is
// returned.
func (r *FleetActionReconciler) removeActionAnnotation(ctx context.Context, action *fleetv1.FleetAction, key string) error {
	cur := action
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			fresh := &fleetv1.FleetAction{}
			if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(action), fresh); err != nil {
				return client.IgnoreNotFound(err)
			}
			cur = fresh
		}
		if _, ok := cur.Annotations[key]; !ok {
			break
		}
		base := cur.DeepCopy()
		delete(cur.Annotations, key)
		err := r.Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil || apierrors.IsNotFound(err) {
			break
		}
		if !apierrors.IsConflict(err) || attempt+1 >= removalAttempts {
			return err
		}
	}
	if cur != action {
		cur.DeepCopyInto(action)
	}
	return nil
}

// removalAttempts bounds removeActionAnnotation's patches of one action in one pass.
const removalAttempts = 5
