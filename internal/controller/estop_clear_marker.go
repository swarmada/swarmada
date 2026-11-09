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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/swarmada/swarmada/internal/estopscope"
)

// dropClearedMarker is a clear's last write: it removes the processed marker of the stop the clear
// pass cleared, the operator stamp that attributed the clear and, when release says so, the stop
// finalizer. obj is the carrier as the pass read it.
//
// The patch is locked to the version obj was read at. A merge patch replaces the annotation map
// when the clear leaves it empty, and the finalizer list whenever it changes, so without the lock a
// trigger written while the clear ran, with the stamp and the finalizer the webhooks add to it,
// would be erased by a patch computed from a copy that never had it. On a conflict the carrier is
// read again from the API server and the pass's decision is taken again on that copy:
//
//   - no trigger, and the marker the pass cleared: the clear has been applied, so only its
//     removals are made again, on that copy;
//   - a trigger: it came in while the clear ran and is a new stop. Only the marker is removed, so
//     the trigger, its stamp and its finalizer stay and the trigger is not read as the stop just
//     cleared, whatever its value. again is true, and the caller runs the pass on cur;
//   - the marker gone or replaced, or the carrier gone: nothing is left to remove.
//
// cur is the carrier as last written or read. An error leaves the marker, so the clear is retried.
// A conflict means the write it lost to has landed, so the carrier is read again at once; after
// clearMarkerAttempts conflicts in a row the conflict is returned.
func dropClearedMarker[T any, PT interface {
	*T
	client.Object
}](ctx context.Context, c client.Client, reader client.Reader, obj PT,
	release func(PT) (bool, error)) (cur PT, again bool, err error) {
	cleared, marked := obj.GetAnnotations()[annEstopProcessed]
	if !marked {
		return obj, false, nil
	}
	cur = obj
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			fresh := PT(new(T))
			if err := reader.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
				if apierrors.IsNotFound(err) {
					return cur, false, nil
				}
				return cur, false, err
			}
			cur = fresh
			if v, ok := cur.GetAnnotations()[annEstopProcessed]; !ok || v != cleared {
				return cur, false, nil
			}
		}
		base := cur.DeepCopyObject().(client.Object)
		ann := cur.GetAnnotations()
		_, firing := ann[annEstopTriggered]
		delete(ann, annEstopProcessed)
		if !firing {
			consumeEstopActor(ann)
		}
		cur.SetAnnotations(ann)
		if !firing {
			drop, err := release(cur)
			if err != nil {
				return cur, false, err
			}
			if drop {
				controllerutil.RemoveFinalizer(cur, estopscope.StopFinalizer)
			}
		}
		err := c.Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil:
			return cur, firing, nil
		case apierrors.IsNotFound(err):
			return cur, false, nil
		case !apierrors.IsConflict(err) || attempt+1 >= clearMarkerAttempts:
			return cur, false, err
		}
	}
}

// clearMarkerAttempts bounds dropClearedMarker's patches of one carrier in one pass.
const clearMarkerAttempts = 5
