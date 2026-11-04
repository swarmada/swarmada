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

// The part of the fake-client problem that a client choice cannot close.
//
// "The FleetTask tests use the fake client, so CEL is never exercised" was
// answered per-rule: someone noticed the quorum rule had no test and wrote
// TestEnvtest_FleetTaskQuorumCELRequired for it. That is the right test, but it is
// a manual mitigation — it depends on the next person remembering. Three of the
// four XValidation rules on FleetTaskSpec had no envtest at all, and nothing said
// so, because a missing admission test is invisible: the fake-client suite accepts
// the object either way and reports green.
//
// So this file does two things. It covers the three unexercised rules, and it adds
// a guard that turns "remember to test your CEL rule" into something the build
// checks: TestFleetTaskCELRulesAllHaveEnvtests reads the markers out of api/v1 and
// fails if a rule is not claimed by a named test here. A rule with a typo, a wrong
// CEL path, or one that CRD regeneration silently dropped behaves exactly like a
// correct one under any client that does not talk to an API server — the only
// place that difference is observable is admission.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// celFixture is a minimal admissible FleetTask the per-rule tests deform.
func celFixture(name, ns string) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: fleetv1.CompletionPolicyAll,
			FailurePolicy:    fleetv1.FailurePolicyFailFast,
			DesiredState:     fleetv1.DesiredStateRunning,
			Actions: []fleetv1.FleetTaskAction{{
				Name:   "a",
				Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			}},
		},
	}
}

func strPtr(s string) *string { return &s }

// Rule 2: spec.flow and a member's dependsOn are mutually exclusive.
//
// The reconciler relies on this and says so — "spec.flow is a SEPARATE scheduling
// path, mutually exclusive with dependsOn (CEL-enforced at admission)" — and then
// branches to reconcileFlow before validateGraph ever runs. If the rule is not in
// force, a task carrying both takes the flow path with a dependency graph nothing
// ever reads, and the dependsOn edges are silently ignored.
func TestEnvtest_FleetTaskFlowExcludesDependsOn(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	bad := celFixture("ft-flow-dependson", ns)
	bad.Spec.Actions = append(bad.Spec.Actions, fleetv1.FleetTaskAction{
		Name:      "b",
		Action:    fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		DependsOn: []string{"a"},
	})
	bad.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}

	err := envK8s.Create(ctx, bad)
	if err == nil {
		t.Fatal("the API server accepted a FleetTask with BOTH spec.flow and a member dependsOn — " +
			"the reconciler branches to reconcileFlow before validateGraph, so those dependsOn edges " +
			"would be silently ignored rather than rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("rejected, but not by the expected rule — message was: %v", err)
	}

	// Not merely rejecting everything: flow alone, and dependsOn alone, are both fine.
	flowOnly := celFixture("ft-flow-only", ns)
	flowOnly.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
	if err := envK8s.Create(ctx, flowOnly); err != nil {
		t.Errorf("the rule rejected a flow-only task: %v", err)
	}
	depsOnly := celFixture("ft-deps-only", ns)
	depsOnly.Spec.Actions = append(depsOnly.Spec.Actions, fleetv1.FleetTaskAction{
		Name:      "b",
		Action:    fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		DependsOn: []string{"a"},
	})
	if err := envK8s.Create(ctx, depsOnly); err != nil {
		t.Errorf("the rule rejected a dependsOn-only task: %v", err)
	}
}

// Rule 3: spec.flow supplies the task's outcome, so a flow may not also carry a
// non-default completionPolicy.
//
// Unenforced, this is the quorum bug's shape again: two sources of truth for
// whether the task succeeded, and the one the author wrote is the one that loses.
func TestEnvtest_FleetTaskFlowExcludesCompletionPolicy(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	two := int32(2)
	for _, bad := range []struct {
		name   string
		policy fleetv1.CompletionPolicy
		quorum *int32
	}{
		{"ft-flow-any", fleetv1.CompletionPolicyAny, nil},
		{"ft-flow-quorum", fleetv1.CompletionPolicyQuorum, &two},
	} {
		ft := celFixture(bad.name, ns)
		ft.Spec.CompletionPolicy = bad.policy
		ft.Spec.Quorum = bad.quorum
		ft.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
		err := envK8s.Create(ctx, ft)
		if err == nil {
			t.Errorf("%s: the API server accepted spec.flow together with completionPolicy %s — "+
				"the flow's root node and the completion policy are then two answers to the same "+
				"question", bad.name, bad.policy)
			continue
		}
		if !strings.Contains(err.Error(), "spec.flow supplies the task's outcome") {
			t.Errorf("%s: rejected, but not by the expected rule — message was: %v", bad.name, err)
		}
	}

	// flow with the default All must still be accepted.
	ok := celFixture("ft-flow-all", ns)
	ok.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
	if err := envK8s.Create(ctx, ok); err != nil {
		t.Errorf("the rule rejected flow + completionPolicy All, which it must permit: %v", err)
	}
}

// Rule 4: a member carrying spec.executor (an External action) may not declare a
// compensation.
//
// This is a safety rule, not a tidiness one: the compensation saga is the
// control plane's promise that a partially-completed task is rolled back, and an
// external executor is precisely the thing the control plane cannot roll back.
// Accepting the field would make the saga claim an undo it cannot perform.
func TestEnvtest_FleetTaskCompensationExcludesExecutor(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	external := fleetv1.FleetActionSpec{
		Type:     fleetv1.ActionTypeExternal,
		Executor: &fleetv1.ActionExecutor{EndpointRef: "scoring-endpoint"},
	}

	bad := celFixture("ft-comp-executor", ns)
	bad.Spec.Actions[0].Action = external
	bad.Spec.Actions[0].Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	err := envK8s.Create(ctx, bad)
	if err == nil {
		t.Fatal("the API server accepted a compensation on a member whose action carries " +
			"spec.executor — the saga would report the external work rolled back when nothing " +
			"in the control plane can roll it back")
	}
	if !strings.Contains(err.Error(), "excluded from Compensate") {
		t.Errorf("rejected, but not by the expected rule — message was: %v", err)
	}

	// Each half alone is valid: an external member without a compensation, and a
	// compensation on an ordinary member.
	extOnly := celFixture("ft-executor-only", ns)
	extOnly.Spec.Actions[0].Action = external
	if err := envK8s.Create(ctx, extOnly); err != nil {
		t.Errorf("the rule rejected an external member with no compensation: %v", err)
	}
	compOnly := celFixture("ft-compensation-only", ns)
	compOnly.Spec.Actions[0].Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	if err := envK8s.Create(ctx, compOnly); err != nil {
		t.Errorf("the rule rejected a compensation on an ordinary member: %v", err)
	}
}

// Rule 5: a compensation carries no executor.
//
// Rule 4 refuses a compensation on an external member; this refuses the other
// direction, an external compensation on a robot member. The reason is the same:
// an external executor writes to its sink on the data path, so an undo it
// performed is one the control plane never observes. has(executor) is tied to
// type External by FleetActionSpec's own rule, so this also refuses a
// compensation of type External.
func TestEnvtest_FleetTaskCompensationCarriesNoExecutor(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	bad := celFixture("ft-comp-external", ns)
	bad.Spec.Actions[0].Compensation = &fleetv1.FleetActionSpec{
		Type:     fleetv1.ActionTypeExternal,
		Executor: &fleetv1.ActionExecutor{EndpointRef: "undo-endpoint"},
	}
	err := envK8s.Create(ctx, bad)
	if err == nil {
		t.Fatal("the API server accepted a compensation carrying spec.executor on a robot " +
			"member — the saga would report an external undo the control plane never observed")
	}
	if !strings.Contains(err.Error(), "spec.compensation must not carry spec.executor") {
		t.Errorf("rejected, but not by the expected rule — message was: %v", err)
	}

	// A compensation without an executor is still admitted.
	ok := celFixture("ft-comp-no-executor", ns)
	ok.Spec.Actions[0].Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	if err := envK8s.Create(ctx, ok); err != nil {
		t.Errorf("the rule rejected a compensation with no executor: %v", err)
	}
}

// ── the guard ────────────────────────────────────────────────────────────────

// celRuleOwners maps each XValidation rule declared on FleetTaskSpec to the
// envtest that exercises it. The key is a distinctive substring of the rule as
// written in api/v1/fleettask_types.go.
//
// Adding a CEL rule without adding an envtest fails TestFleetTaskCELRulesAllHaveEnvtests.
// That is the obligation the per-rule mitigation could not express: someone had to
// remember, and nothing checked that they had.
var celRuleOwners = map[string]string{
	"has(self.quorum)":               "TestEnvtest_FleetTaskQuorumCELRequired",
	"size(a.dependsOn) == 0":         "TestEnvtest_FleetTaskFlowExcludesDependsOn",
	"self.completionPolicy == 'All'": "TestEnvtest_FleetTaskFlowExcludesCompletionPolicy",
	"!has(a.action.executor)":        "TestEnvtest_FleetTaskCompensationExcludesExecutor",
	"!has(a.compensation.executor)":  "TestEnvtest_FleetTaskCompensationCarriesNoExecutor",
}

var xvalidationRule = regexp.MustCompile(`\+kubebuilder:validation:XValidation:rule="((?:[^"\\]|\\.)*)"`)

// TestFleetTaskCELRulesAllHaveEnvtests asserts that every XValidation rule on
// FleetTaskSpec is claimed by a test in this package, and that the claimed test
// actually exists. It reads the source rather than the CRD YAML deliberately: the
// marker is what an author writes, and the failure this guards against is a rule
// being added (or edited) with no admission test following it.
//
// It does not need the envtest control plane — it is a source-level obligation —
// so it runs everywhere, including where the per-rule tests would skip.
func TestFleetTaskCELRulesAllHaveEnvtests(t *testing.T) {
	src, err := os.ReadFile("../../api/v1/fleettask_types.go")
	if err != nil {
		t.Fatalf("reading api/v1/fleettask_types.go: %v", err)
	}
	// Only the markers attached to FleetTaskSpec; the file declares other types.
	specDecl := strings.Index(string(src), "\ntype FleetTaskSpec struct {")
	if specDecl < 0 {
		t.Fatal("could not find `type FleetTaskSpec struct` — this guard is reading the wrong file")
	}
	block := string(src)[:specDecl]
	if i := strings.LastIndex(block, "\n\n"); i >= 0 {
		block = block[i:] // the contiguous doc/marker comment immediately above the type
	}

	found := map[string]bool{}
	for _, m := range xvalidationRule.FindAllStringSubmatch(block, -1) {
		rule := m[1]
		owner := ""
		for key, test := range celRuleOwners {
			if strings.Contains(rule, key) {
				owner, found[key] = test, true
				break
			}
		}
		if owner == "" {
			t.Errorf("XValidation rule on FleetTaskSpec has no envtest exercising it:\n  %s\n"+
				"Add an envtest that creates an object the rule must reject (and one it must "+
				"accept), then register it in celRuleOwners. A CEL rule with a typo, a wrong path, "+
				"or one dropped by CRD regeneration is indistinguishable from a correct one "+
				"anywhere except admission.", rule)
		}
	}
	if len(found) == 0 {
		t.Fatal("no XValidation markers found above FleetTaskSpec — either the markers were " +
			"removed (in which case admission is no longer enforcing anything this suite assumes) " +
			"or this guard's parsing has drifted from the file")
	}
	for key, test := range celRuleOwners {
		if !found[key] {
			t.Errorf("celRuleOwners claims %s covers a rule containing %q, but no such rule is "+
				"declared on FleetTaskSpec any more — the rule was removed or rewritten and its "+
				"test is now asserting nothing", test, key)
		}
	}
	// The named owners must be real test functions in this package.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	var all strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_test.go") {
			b, err := os.ReadFile(e.Name())
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			all.Write(b)
		}
	}
	for key, test := range celRuleOwners {
		if !strings.Contains(all.String(), "func "+test+"(") {
			t.Errorf("celRuleOwners maps %q to %s, which does not exist in this package", key, test)
		}
	}
}
