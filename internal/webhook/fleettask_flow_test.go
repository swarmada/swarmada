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

// spec.flow's tree shape cannot be expressed in the CRD schema (`loop` is a
// CEL reserved word; structural schemas do not support true recursion past
// depth 1 — see api/v1/fleettask_flow_types.go and internal/flow/validate.go)
// — so ValidateCreate/ValidateUpdate are the ONLY admission-time check for
// it. These tests exercise that boundary directly, the same way
// fleettask_append_authz_test.go exercises the append-only boundary.

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func ftWithFlow(name string, flow *fleetv1.FlowNode, actions ...fleetv1.FleetTaskAction) *fleetv1.FleetTask {
	t := ftWith(name, actions...)
	t.Spec.Flow = flow
	return t
}

func runNode(member string) fleetv1.FlowNode { return fleetv1.FlowNode{Run: &member} }

// THE CRITICAL ONE: an unbounded Forever loop must be rejected at CREATE —
// "Admission rejects otherwise" is the literal, non-negotiable rule.
func TestFleetTaskCreate_UnboundedForeverLoopRejected(t *testing.T) {
	v := newFTValidator(nil)
	task := ftWithFlow("t1",
		&fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, Body: runNode("a")}},
		ftAct("a"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("an unbounded Forever loop must be rejected at admission")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error, got: %v", err)
	}
}

// A `run:` referencing a member not in spec.actions is rejected — no CEL rule
// over an arbitrarily deep tree could express this either way, so it is
// entirely this webhook's job.
func TestFleetTaskCreate_RunReferencesUnknownMemberRejected(t *testing.T) {
	v := newFTValidator(nil)
	task := ftWithFlow("t1", &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("a"), runNode("does-not-exist")}}, ftAct("a"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error naming the unknown member, got: %v", err)
	}
}

// A valid flow tree is admitted.
func TestFleetTaskCreate_ValidFlowAdmitted(t *testing.T) {
	v := newFTValidator(nil)
	times := int32(12)
	task := ftWithFlow("t1",
		&fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &times, Body: runNode("patrol")}},
		ftAct("patrol"))

	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("a valid flow tree must be admitted, got: %v", err)
	}
}

// A task with no flow at all (the v0.3 shape) is unaffected.
func TestFleetTaskCreate_NoFlowAdmitted(t *testing.T) {
	v := newFTValidator(nil)
	task := ftWith("t1", ftAct("a"))

	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("a task with no flow must be admitted, got: %v", err)
	}
}

// The SAME check runs on UPDATE: nothing else constrains spec.flow, so an
// edit that breaks it must not slip through just because the object already
// existed.
func TestFleetTaskUpdate_InvalidFlowRejected(t *testing.T) {
	v := newFTValidator(&fakeEstopAuthz{allow: true})
	oldTask := ftWith("t1", ftAct("a"))
	newTask := ftWithFlow("t1", &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "a", Field: "label"},
		Default: fleetv1.FlowNode{}, // no cases, but that alone is valid — break it via an empty predicate case below
	}}, ftAct("a"))
	newTask.Spec.Flow.Select.Cases = []fleetv1.SelectCase{{}} // no predicate at all

	_, err := v.ValidateUpdate(ctxAsUser("op"), oldTask, newTask)
	if err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("an update introducing an invalid flow must be rejected, got: %v", err)
	}
}
