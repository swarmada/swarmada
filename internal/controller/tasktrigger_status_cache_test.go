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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// draftCacheLag is a client whose reads have not yet seen one FleetTask, which is what
// the manager's cache looks like in the moment after a draft is created and before
// its watch event arrives. Writes and every other read pass through.
type draftCacheLag struct {
	client.Client
	unseen string
}

func (l draftCacheLag) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*fleetv1.FleetTask); ok && key.Name == l.unseen {
		return apierrors.NewNotFound(schema.GroupResource{Group: "swarmada.io", Resource: "fleettasks"}, key.Name)
	}
	return l.Client.Get(ctx, key, obj, opts...)
}

// A draft the cache has not caught up with is not a deleted draft. Pruning on a cache
// miss alone would take a just-drafted task off the queue a person works from, so a
// miss is confirmed against the API server before anything is removed.
func TestTrigger_PruningDoesNotDropADraftTheCacheHasNotSeenYet(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-2", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	r.Client = draftCacheLag{Client: c, unseen: "intruder-response-ev-1"}
	r.APIReader = c
	reconcileEvent(t, r, "ev-2")

	got := getTrigger(t, c, "intruder-response").Status.PendingDrafts
	if !containsString(got, "intruder-response-ev-1") {
		t.Errorf("pendingDrafts = %v: a held draft was dropped because the cache had not seen it yet", got)
	}
}

// countingReader counts the reads that bypass the cache.
type countingReader struct {
	client.Reader
	gets *int
}

func (c countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	*c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

// The uncached reader confirms a cache miss on a draft and does nothing else. With the
// cache in step, a run of events, repeated passes, a cleared draft and a deleted draft
// make no uncached read at all; with one draft missing from the cache, each prune pass
// makes exactly one, for that draft.
func TestTrigger_APIReaderIsUsedOnlyToConfirmACacheMiss(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-2", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-3", "yard-sensors", "IntruderDetected"),
	)
	gets := 0
	r.APIReader = countingReader{Reader: c, gets: &gets}

	for _, ev := range []string{"ev-1", "ev-2", "ev-3", "ev-1", "ev-2", "ev-3"} {
		reconcileEvent(t, r, ev)
	}
	forgetEvaluation(t, c, "ev-2")
	reconcileEvent(t, r, "ev-2")
	clearDraft(t, c, "intruder-response-ev-1")
	reconcileEvent(t, r, "ev-1")
	if gets != 0 {
		t.Fatalf("%d uncached read(s) with the cache in step; the API reader is for confirming a cache miss only", gets)
	}

	// The deleted draft is a real miss: confirmed once, then gone from the queue, and
	// never read again.
	if err := c.Delete(context.Background(), getTask(t, c, "intruder-response-ev-2")); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	reconcileEvent(t, r, "ev-2")
	if gets != 1 {
		t.Fatalf("uncached reads = %d after one draft was deleted, want 1 (the confirmation)", gets)
	}
	for i := 0; i < 3; i++ {
		reconcileEvent(t, r, "ev-3")
	}
	if gets != 1 {
		t.Errorf("uncached reads = %d after further passes, want still 1: a pruned draft is not looked up again", gets)
	}

	// A draft the cache has not seen: one confirmation per pass that meets it.
	r.Client = draftCacheLag{Client: c, unseen: "intruder-response-ev-3"}
	reconcileEvent(t, r, "ev-3")
	if gets != 2 {
		t.Errorf("uncached reads = %d with one draft missing from the cache, want 2", gets)
	}
}
