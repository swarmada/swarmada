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
	stderrors "errors"
	"math"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/swarmada/swarmada/internal/executorguard"
)

// Every generation in a namespace is drawn from its store, so it rises across actions and across an
// action created again under its name, and reads no clock once the store exists: a store whose mark
// is far ahead of any clock (as after the clock steps back) is still the floor.
func TestMintGeneration_DrawsEveryGenerationFromTheNamespaceStore(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("a"), pendingAction("b"))
	ctx := context.Background()
	mint := func(name string) int64 {
		t.Helper()
		fa := getAction(t, c, name)
		gen, err := r.mintGeneration(ctx, fa)
		if err != nil {
			t.Fatalf("minting for %s: %v", name, err)
		}
		return gen
	}
	a1 := mint("a")
	b1 := mint("b")
	a2 := mint("a") // a's previous generation is still 0 here: the store alone orders it
	if a1 >= b1 || b1 >= a2 {
		t.Fatalf("generations %d, %d, %d do not rise across actions", a1, b1, a2)
	}

	ahead := int64(1) << 61
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(leaseGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: fencingStoreName}, store); err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	store.SetAnnotations(map[string]string{annFencingHighWater: strconv.FormatInt(ahead, 10)})
	if err := c.Update(ctx, store); err != nil {
		t.Fatalf("moving the mark ahead: %v", err)
	}
	if got := mint("b"); got != ahead+1 {
		t.Fatalf("generation %d after a mark of %d: the store, not the clock, must decide", got, ahead)
	}

	// An action created again under a name starts from the store, never from 0.
	again := pendingAction("a")
	again.Status.AssignmentGeneration = 0
	if gen, err := r.mintGeneration(ctx, again); err != nil || gen != ahead+2 {
		t.Fatalf("the re-created action got %d (err %v), want %d", gen, err, ahead+2)
	}
}

// setMark writes mark into the store kept at key.
func setMark(t *testing.T, c client.Client, key types.NamespacedName, mark string) {
	t.Helper()
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(leaseGVK)
	if err := c.Get(context.Background(), key, store); err != nil {
		t.Fatalf("reading the store %s: %v", key, err)
	}
	store.SetAnnotations(map[string]string{annFencingHighWater: mark})
	if err := c.Update(context.Background(), store); err != nil {
		t.Fatalf("writing the store %s: %v", key, err)
	}
}

// The store is kept in the control plane's namespace, named for the namespace it serves.
func TestMintGeneration_KeepsTheStoreInTheControlPlaneNamespace(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("a"))
	r.GenerationStoreNamespace = "swarmada-system"
	if _, err := r.mintGeneration(context.Background(), getAction(t, c, "a")); err != nil {
		t.Fatalf("minting: %v", err)
	}
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(leaseGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "swarmada-system", Name: fencingStorePrefix + actionNS}, store); err != nil {
		t.Fatalf("the store is not in the control plane's namespace: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: fencingStoreName}, store); err == nil {
		t.Fatal("a store was written in the namespace it serves")
	}
}

// A store lowered below a generation an action holds is refused and raised, never drawn from.
func TestMintGeneration_RefusesAStoreBelowAGenerationIssued(t *testing.T) {
	held := pendingAction("held")
	held.Status.AssignmentGeneration = 5000
	r, c := newActionReconciler(t, pendingAction("a"), held)
	ctx := context.Background()
	if _, err := r.mintGeneration(ctx, getAction(t, c, "a")); err != nil {
		t.Fatalf("minting: %v", err)
	}
	key := r.storeKey(actionNS)
	setMark(t, c, key, "10")
	r.minted = generationMemory{} // a restart: only the actions remember
	_, err := r.mintGeneration(ctx, getAction(t, c, "a"))
	var behind errStoreBehind
	if !stderrors.As(err, &behind) || behind.mark != 10 || behind.floor != 5000 {
		t.Fatalf("minting from a store lowered to 10 under a held 5000: err %v, want errStoreBehind{10, 5000}", err)
	}
	if gen, err := r.mintGeneration(ctx, getAction(t, c, "a")); err != nil || gen != 5001 {
		t.Fatalf("after the store was raised: %d, %v; want 5001", gen, err)
	}
}

// A generation past the ceiling is never issued, and a store that reads above it is refused.
func TestMintGeneration_RefusesPastTheCeiling(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("a"))
	ctx := context.Background()
	if _, err := r.mintGeneration(ctx, getAction(t, c, "a")); err != nil {
		t.Fatalf("minting: %v", err)
	}
	key := r.storeKey(actionNS)
	var ceiling errAtCeiling
	for _, mark := range []int64{executorguard.GenerationCeiling, executorguard.GenerationCeiling + 1, math.MaxInt64} {
		setMark(t, c, key, strconv.FormatInt(mark, 10))
		if gen, err := r.mintGeneration(ctx, getAction(t, c, "a")); !stderrors.As(err, &ceiling) {
			t.Errorf("store at %d: minted %d (err %v), want errAtCeiling", mark, gen, err)
		}
	}
}
