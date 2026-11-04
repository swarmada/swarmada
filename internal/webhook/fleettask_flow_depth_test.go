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

// at the boundary it actually matters on. The four node-level CEL
// rules bind at depth 1 only — every recursion point generates as
// x-kubernetes-preserve-unknown-fields — so the API server served the message
// "an unbounded loop is not admitted" while admitting exactly that
// for every loop below the root. These tests assert the admission verdict, not
// the helper's: internal/flow/validate_test.go covers the rules themselves.

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// A loop two levels down that omits `mode`: unseen by the CRD enum, skipped by
// the Forever bound test (which keys on the literal value), and run as Forever
// by the evaluator. It must be refused at admission.
func TestFleetTaskCreate_NestedLoopWithoutModeRejected(t *testing.T) {
	v := newFTValidator(nil)
	task := ftWithFlow("t1", &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{
		{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeOnce, Body: fleetv1.FlowNode{
			Loop: &fleetv1.LoopNode{Body: runNode("patrol")}, // no mode, at depth 3
		}}},
	}}, ftAct("patrol"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a nested loop with no mode must be rejected at admission — it runs unbounded")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got: %v", err)
	}
}

// A nested Forever loop with no bound: the message the API server already
// serves must be true below the root, which is the whole of the depth-1 gap.
func TestFleetTaskCreate_NestedUnboundedForeverRejected(t *testing.T) {
	v := newFTValidator(nil)
	task := ftWithFlow("t1", &fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
		Branches: []fleetv1.FlowNode{
			{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, Body: runNode("patrol")}},
		},
	}}, ftAct("patrol"))

	if _, err := v.ValidateCreate(context.Background(), task); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error for a nested unbounded Forever loop, got: %v", err)
	}
}

// The ceilings are admission-time, not reconcile-time discoveries.
func TestFleetTaskCreate_TreePastCeilingsRejected(t *testing.T) {
	v := newFTValidator(nil)

	deep := runNode("patrol")
	for i := 1; i <= flow.MaxDepth; i++ { // one level past the ceiling
		deep = fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{deep}}
	}
	if _, err := v.ValidateCreate(context.Background(), ftWithFlow("t1", &deep, ftAct("patrol"))); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error for a tree past the depth ceiling, got: %v", err)
	}

	wide := make([]fleetv1.FlowNode, 0, flow.MaxNodes)
	for i := 0; i < flow.MaxNodes; i++ { // root + MaxNodes leaves
		wide = append(wide, runNode("patrol"))
	}
	if _, err := v.ValidateCreate(context.Background(), ftWithFlow("t1", &fleetv1.FlowNode{Sequence: wide}, ftAct("patrol"))); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error for a tree past the node ceiling, got: %v", err)
	}
}
