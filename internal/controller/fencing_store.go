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
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// The generation store (RFC-0001 #control-plane-scheduler): one counter per namespace, from which
// every assignment generation, and so every fencing token, in that namespace is drawn. It is a
// coordination.k8s.io Lease the control plane owns, holding its high-water mark in an annotation,
// kept in the control plane's own namespace (GenerationStoreNamespace), where the users of a
// namespace cannot write. One strictly increasing counter makes the tokens a robot is given rise
// across all its actions, and keeps an action created again under a name from repeating or
// undercutting a token an earlier one used.
const (
	fencingStoreName       = "swarmada-fencing-tokens"
	fencingStorePrefix     = "fencing-tokens."
	annFencingHighWater    = "swarmada.io/fencing-high-water"
	labelFencingStoreOwner = "swarmada.io/generation-store-for"

	// EventGenerationStoreBehind: the store read lower than a generation already issued in its
	// namespace; the assignment was refused and the store raised.
	EventGenerationStoreBehind = "GenerationStoreBehind"
	// EventGenerationCeilingReached: the next generation would pass GenerationCeiling; nothing
	// is assigned in the namespace.
	EventGenerationCeilingReached = "GenerationCeilingReached"
)

var leaseGVK = schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"}

// errStoreBehind and errAtCeiling refuse an assignment: the first after raising the store to the
// floor it read below, the second for good, since a generation past the ceiling is never issued.
type errStoreBehind struct{ mark, floor int64 }

func (e errStoreBehind) Error() string {
	return fmt.Sprintf("the generation store read %d, below %d already issued in the namespace; it was raised "+
		"to %d and the assignment is tried again", e.mark, e.floor, e.floor)
}

type errAtCeiling struct{ next int64 }

func (e errAtCeiling) Error() string {
	return fmt.Sprintf("the next assignment generation, %d, would pass the ceiling %d: nothing more is assigned "+
		"in this namespace", e.next, executorguard.GenerationCeiling)
}

// generationMemory is the highest generation this process has issued per namespace: a floor the
// store must not read below, whatever was deleted since. Zero value is usable.
type generationMemory struct {
	mu      sync.Mutex
	highest map[string]int64
}

func (m *generationMemory) get(ns string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.highest[ns]
}

func (m *generationMemory) record(ns string, gen int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.highest == nil {
		m.highest = map[string]int64{}
	}
	m.highest[ns] = max(m.highest[ns], gen)
}

// storeKey is where namespace's counter is kept: in the control plane's own namespace when one is
// set, named for the namespace it serves; else (tests that run no manager) in namespace itself.
func (r *FleetActionReconciler) storeKey(namespace string) types.NamespacedName {
	if r.GenerationStoreNamespace != "" {
		return types.NamespacedName{Namespace: r.GenerationStoreNamespace, Name: fencingStorePrefix + namespace}
	}
	return types.NamespacedName{Namespace: namespace, Name: fencingStoreName}
}

// mintGeneration reserves the generation action's next assignment carries: one above both the
// namespace's mark and the action's previous generation, written back to the store with the
// resourceVersion it read, before the assignment is committed. The store must not read below the
// floor (every generation an action in the namespace holds, and every one this process issued);
// one that does is raised to the floor and the assignment refused this time. A generation past
// the ceiling is never issued. A conflict is returned as one, so the caller requeues; a
// generation reserved for an assignment that is then not committed is a gap, never a repeat.
func (r *FleetActionReconciler) mintGeneration(ctx context.Context, action *fleetv1.FleetAction) (int64, error) {
	floor, err := r.generationFloor(ctx, action.Namespace)
	if err != nil {
		return 0, err
	}
	key := r.storeKey(action.Namespace)
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(leaseGVK)
	err = r.liveReader().Get(ctx, key, store)
	if errors.IsNotFound(err) {
		next := executorguard.NextGeneration(executorguard.SeedGeneration(floor, time.Now()), action.Status.AssignmentGeneration)
		if next > executorguard.GenerationCeiling {
			return 0, errAtCeiling{next: next}
		}
		store = &unstructured.Unstructured{}
		store.SetGroupVersionKind(leaseGVK)
		store.SetNamespace(key.Namespace)
		store.SetName(key.Name)
		store.SetLabels(map[string]string{labelFencingStoreOwner: action.Namespace})
		store.SetAnnotations(map[string]string{annFencingHighWater: strconv.FormatInt(next, 10)})
		if err := r.Create(ctx, store); err != nil {
			if errors.IsAlreadyExists(err) {
				return 0, errors.NewConflict(schema.GroupResource{Group: leaseGVK.Group, Resource: "leases"}, key.Name, err)
			}
			return 0, fmt.Errorf("creating the generation store: %w", err)
		}
		r.minted.record(action.Namespace, next)
		return next, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the generation store: %w", err)
	}
	mark, perr := strconv.ParseInt(store.GetAnnotations()[annFencingHighWater], 10, 64)
	base := store.DeepCopy()
	switch {
	case perr != nil || mark < 0:
		// An unreadable mark is seeded again, as a missing store is.
		mark = executorguard.SeedGeneration(floor, time.Now())
	case mark > executorguard.GenerationCeiling:
		return 0, errAtCeiling{next: mark}
	case mark < floor:
		if err := r.writeMark(ctx, store, base, floor); err != nil {
			return 0, err
		}
		return 0, errStoreBehind{mark: mark, floor: floor}
	}
	next := executorguard.NextGeneration(mark, action.Status.AssignmentGeneration)
	if next > executorguard.GenerationCeiling {
		return 0, errAtCeiling{next: next}
	}
	if err := r.writeMark(ctx, store, base, next); err != nil {
		return 0, err
	}
	r.minted.record(action.Namespace, next)
	return next, nil
}

// writeMark writes mark into store with the resourceVersion base was read at.
func (r *FleetActionReconciler) writeMark(ctx context.Context, store, base *unstructured.Unstructured, mark int64) error {
	ann := store.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[annFencingHighWater] = strconv.FormatInt(mark, 10)
	store.SetAnnotations(ann)
	if err := r.Patch(ctx, store, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsConflict(err) {
			return err
		}
		return fmt.Errorf("writing the generation store: %w", err)
	}
	return nil
}

// generationFloor is the lowest mark the store may hold for namespace: the highest generation an
// action in it holds, and the highest this process has issued there.
func (r *FleetActionReconciler) generationFloor(ctx context.Context, namespace string) (int64, error) {
	var actions fleetv1.FleetActionList
	if err := r.List(ctx, &actions, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("listing actions for the generation floor: %w", err)
	}
	highest := r.minted.get(namespace)
	for i := range actions.Items {
		highest = max(highest, actions.Items[i].Status.AssignmentGeneration)
	}
	return highest, nil
}
