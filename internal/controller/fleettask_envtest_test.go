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

// envtest harness for the FleetTask reconciler, and the branch tests that need it.
//
// WHY THIS FILE EXISTS. The FleetTask suite was written against
// sigs.k8s.io/controller-runtime/pkg/client/fake, which performs no admission at
// all: it accepts objects the API server would reject, defaults nothing, and
// invents its own resourceVersion semantics. A test written on that client can
// assert a reconcile path that cannot occur in a cluster, and nothing says so.
//
// Every fixture below is created through a real API server with the generated
// CRDs installed, so it has to survive the enum checks, the MinItems/Minimum
// bounds, the four XValidation rules on FleetTaskSpec, and CRD defaulting before
// the reconciler ever sees it. Three of the tests here are meaningless on the
// fake client and are only assertions at all because of that:
//
//   - TestFleetTask_UnknownDependencyIsFailed proves admission does NOT catch an
//     unknown dependsOn, which is the only thing that makes validateGraph's
//     belt-and-suspenders arm load-bearing rather than dead code.
//   - TestFleetTask_ReconcileIsIdempotent reads resourceVersion, which only means
//     something against an API server that assigns it.
//   - TestFleetTask_StartCondition* rely on CRD defaulting to establish that the
//     unset value really is Succeeded, which is what the rest of the suite assumes.

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ── harness ──────────────────────────────────────────────────────────────────

// ftEnv is a FleetTask reconciler bound to a fresh envtest namespace. It mirrors
// the fake-client helpers (newFleetTaskReconciler / reconcileFT / getFT /
// getActionOrNil) so a fixture reads the same, but every object round-trips
// through the API server.
type ftEnv struct {
	t  *testing.T
	ns string
	r  *FleetTaskReconciler
}

// newFTEnv creates the given objects in an isolated namespace and returns the
// harness. Status is not persisted by Create (it is a subresource), so any
// FleetAction fixture carrying a status.phase is re-applied with a status write —
// the same two-step a real controller performs, and a difference the fake client
// hides.
func newFTEnv(t *testing.T, objs ...client.Object) *ftEnv {
	t.Helper()
	requireEnvtest(t)
	e := &ftEnv{t: t, ns: envtestNamespace(t)}
	e.r = &FleetTaskReconciler{Client: envK8s, Scheme: envScheme}
	for _, o := range objs {
		e.createObject(o)
	}
	return e
}

func (e *ftEnv) createObject(o client.Object) {
	e.t.Helper()
	ctx := context.Background()
	o.SetNamespace(e.ns)
	var wantPhase fleetv1.ActionPhase
	if a, ok := o.(*fleetv1.FleetAction); ok {
		wantPhase = a.Status.Phase
		// A seeded child names its task, which the API server gave its own UID on create.
		for i, ref := range a.OwnerReferences {
			var owner fleetv1.FleetTask
			if ref.Kind == "FleetTask" && envK8s.Get(ctx, types.NamespacedName{Namespace: e.ns, Name: ref.Name}, &owner) == nil {
				a.OwnerReferences[i].UID = owner.UID
			}
		}
	}
	if err := envK8s.Create(ctx, o); err != nil {
		e.t.Fatalf("creating %T %q through the API server: %v — the fixture does not pass admission",
			o, o.GetName(), err)
	}
	if wantPhase != "" {
		e.setPhase(o.GetName(), wantPhase)
	}
}

// setPhase drives a child (primary or compensation) FleetAction to a phase via the
// status subresource.
func (e *ftEnv) setPhase(name string, phase fleetv1.ActionPhase) {
	e.t.Helper()
	ctx := context.Background()
	var a fleetv1.FleetAction
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: e.ns, Name: name}, &a); err != nil {
		e.t.Fatalf("get action %s: %v", name, err)
	}
	a.Status.Phase = phase
	if err := envK8s.Status().Update(ctx, &a); err != nil {
		e.t.Fatalf("update %s status: %v", name, err)
	}
}

func (e *ftEnv) reconcile(name string) {
	e.t.Helper()
	if _, err := e.r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: name}}); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
}

func (e *ftEnv) task(name string) *fleetv1.FleetTask {
	e.t.Helper()
	var ft fleetv1.FleetTask
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: e.ns, Name: name}, &ft); err != nil {
		e.t.Fatalf("get fleettask: %v", err)
	}
	return &ft
}

// action returns the named FleetAction, or nil when it does not exist.
func (e *ftEnv) action(name string) *fleetv1.FleetAction {
	e.t.Helper()
	var a fleetv1.FleetAction
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: e.ns, Name: name}, &a); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		e.t.Fatalf("get action: %v", err)
	}
	return &a
}

// ftActionTrigger is ftAction with the trigger mode exposed. The shared ftAction
// helper hard-codes TriggerModeAuto, so OnEvent had no way in.
func ftActionTrigger(name string, deps []string, sc fleetv1.ActionPhase, tm fleetv1.TriggerMode) fleetv1.FleetTaskAction {
	a := ftAction(name, deps, sc)
	a.Trigger = tm
	return a
}

// ── validateGraph's unknown-dependency branch ────────────────────────────────

// validateGraph has two failure returns and one condition. The cycle return had a
// test; the unknown-dependency return, which fires before the DFS is even built,
// had none — so a passing DependencyGraphValid=False said nothing about which of
// the two produced it. This asserts the unknown-dependency return specifically, by
// its message.
//
// It is on envtest for a reason the fake client cannot supply: it also proves that
// admission does NOT reject a dependsOn naming a non-existent member. None of the
// four XValidation rules on FleetTaskSpec checks dependsOn membership, so the
// controller-side check is the only thing standing between an operator's typo and
// a task that never starts and never says why.
func TestFleetTask_UnknownDependencyIsFailed(t *testing.T) {
	task := fleetTask("ft-unknown-dep", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"stage-c"}, fleetv1.ActionPhaseSucceeded), // "stage-c" is not declared
	)
	e := newFTEnv(t, task)
	e.reconcile("ft-unknown-dep")

	ft := e.task("ft-unknown-dep")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed: a dependsOn naming an undeclared action can never be "+
			"satisfied, so the task must fail rather than wait forever", ft.Status.Phase)
	}
	cond := condOf(ft, "DependencyGraphValid")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("DependencyGraphValid = %v, want False", cond)
	}
	// The message is the only thing that distinguishes this return from the cycle
	// return; both write the same condition type, status and reason.
	if !strings.Contains(cond.Message, `dependsOn unknown action "stage-c"`) {
		t.Errorf("condition message = %q, want it to name the unknown action — otherwise this test "+
			"cannot tell the unknown-dependency return from the cycle return", cond.Message)
	}
	// Nothing may be generated for an invalid graph, not even the valid root.
	if e.action(childName("ft-unknown-dep", "a")) != nil {
		t.Errorf("child 'a' was generated for a task whose graph is invalid")
	}
}

// ── startCondition Assigned and InProgress ───────────────────────────────────

// The entire point of a settable startCondition is starting a dependent BEFORE its
// predecessor finishes. Every fixture in the suite passed Succeeded, so the two
// values that produce that behaviour — Assigned and InProgress — had never run.
//
// Each case asserts both halves: the dependent IS generated at its declared
// condition, and is NOT generated one rank below it. Without the negative half a
// test that generated everything unconditionally would still pass.
func TestFleetTask_StartConditionInProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		// start is the dependent's startCondition; tooEarly is a predecessor phase
		// that must NOT release it; releases is the phase that must.
		start    fleetv1.ActionPhase
		tooEarly fleetv1.ActionPhase
		releases fleetv1.ActionPhase
	}{
		{"assigned", fleetv1.ActionPhaseAssigned, fleetv1.ActionPhasePending, fleetv1.ActionPhaseAssigned},
		{"inprogress", fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress},
		// The control: the default. Proves the harness is not simply generating
		// every member, and pins CRD defaulting — the value the other 30-odd
		// fixtures in this package assume without ever asserting.
		{"succeeded-default", "", fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := fleetTask("ft-sc", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
				fleetv1.DesiredStateRunning,
				ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
				ftAction("b", []string{"a"}, tc.start),
			)
			e := newFTEnv(t, task)

			// CRD defaulting: an unset startCondition must come back as Succeeded.
			stored := e.task("ft-sc")
			if tc.start == "" && stored.Spec.Actions[1].StartCondition != fleetv1.ActionPhaseSucceeded {
				t.Fatalf("unset startCondition defaulted to %q, want Succeeded",
					stored.Spec.Actions[1].StartCondition)
			}

			e.reconcile("ft-sc") // generates 'a'
			if e.action(childName("ft-sc", "a")) == nil {
				t.Fatal("root action 'a' was not generated")
			}

			e.setPhase(childName("ft-sc", "a"), tc.tooEarly)
			e.reconcile("ft-sc")
			if e.action(childName("ft-sc", "b")) != nil {
				t.Fatalf("'b' (startCondition %q) was released at predecessor phase %q, one rank too early",
					tc.start, tc.tooEarly)
			}

			e.setPhase(childName("ft-sc", "a"), tc.releases)
			e.reconcile("ft-sc")
			if e.action(childName("ft-sc", "b")) == nil {
				t.Fatalf("'b' (startCondition %q) was NOT released at predecessor phase %q — a settable "+
					"startCondition that still waits for Succeeded is the field doing nothing",
					tc.start, tc.releases)
			}
			// And the predecessor really has not finished: that is the behaviour.
			if got := e.action(childName("ft-sc", "a")).Status.Phase; tc.start != "" && got == fleetv1.ActionPhaseSucceeded {
				t.Fatalf("predecessor reached Succeeded (%q); the test no longer proves an early start", got)
			}
			if !dependenciesMetIn(e.task("ft-sc"), "b") {
				t.Errorf("status.actions[b].dependenciesMet is false although 'b' was generated")
			}
		})
	}
}

// dependenciesMetIn reads the projected per-member dependenciesMet flag.
func dependenciesMetIn(ft *fleetv1.FleetTask, name string) bool {
	for i := range ft.Status.Actions {
		if ft.Status.Actions[i].Name == name {
			return ft.Status.Actions[i].DependenciesMet
		}
	}
	return false
}

// ── trigger OnEvent dormancy ─────────────────────────────────────────────────

// OnEvent is a first-class enum value with a dedicated skip in the generation
// loop, and no test had ever set it: the shared ftAction helper hard-codes Auto
// and does not take the field. The skip was reachable in production and
// unreachable in test.
func TestFleetTask_OnEventStaysDormant(t *testing.T) {
	task := fleetTask("ft-onevent", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftActionTrigger("auto", nil, fleetv1.ActionPhaseSucceeded, fleetv1.TriggerModeAuto),
		// Dormant despite having NO dependencies at all: nothing is holding it
		// back except the trigger mode, which is the whole assertion.
		ftActionTrigger("onevent", nil, fleetv1.ActionPhaseSucceeded, fleetv1.TriggerModeOnEvent),
	)
	e := newFTEnv(t, task)

	if got := e.task("ft-onevent").Spec.Actions[1].Trigger; got != fleetv1.TriggerModeOnEvent {
		t.Fatalf("trigger stored as %q, want OnEvent — the API server did not keep the value", got)
	}

	e.reconcile("ft-onevent")

	if e.action(childName("ft-onevent", "auto")) == nil {
		t.Fatal("the Auto member was not generated; the fixture proves nothing about OnEvent")
	}
	if e.action(childName("ft-onevent", "onevent")) != nil {
		t.Fatal("a trigger: OnEvent member was generated at reconcile — dormancy means no child " +
			"until something activates it, and generating it dispatches work to a robot nobody asked for")
	}

	// Dormant is not terminal: the member holds the task open rather than being
	// counted as lost. Driving the Auto member to Succeeded must NOT complete an
	// All task that still has an unactivated member.
	e.setPhase(childName("ft-onevent", "auto"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-onevent")
	ft := e.task("ft-onevent")
	if ft.Status.Phase == fleetv1.FleetTaskPhaseSucceeded {
		t.Fatal("completionPolicy All reported Succeeded with a dormant member never run")
	}
	if ft.Status.Phase != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running while a dormant member is outstanding", ft.Status.Phase)
	}
	if got := ft.Status.ActionSummary; got != "1/2 Succeeded" {
		t.Errorf("actionSummary = %q, want \"1/2 Succeeded\"", got)
	}
	// Still no child, on a second pass: dormancy is level-triggered, not a
	// one-shot the first reconcile happened to skip.
	if e.action(childName("ft-onevent", "onevent")) != nil {
		t.Error("the OnEvent member was generated on a later reconcile")
	}
}

// ── the unmeetable-policy fork ───────────────────────────────────────────────
//
// One fixture, two failure policies. Quorum 2 over three members with two already
// Failed and one still InProgress: succeeded 0 + nonTerminal 1 = 1, below the
// threshold of 2, so the policy can never be met while work is still in flight.
// That is the only state in which aggregate() reaches the !stillPossible() fork,
// and neither arm of it had ever executed.
//
// Reaching the fork at all needs !completionMet() AND nonTerminal > 0 AND
// !stillPossible(). Every prior Quorum fixture returned early at completionMet(),
// or returned true from stillPossible().

func quorumDoomed(name string, fp fleetv1.FailurePolicy) *fleetv1.FleetTask {
	q := int32(2)
	task := fleetTask(name, fleetv1.CompletionPolicyQuorum, fp, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("c", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = &q
	return task
}

// FailFast: a quorum that can no longer be reached is failed promptly and its
// in-flight member is confirmed-cancelled, so no robot keeps a lease on work whose
// task is already lost. The alternative — the false branch going unasserted — is a
// doomed task left Running forever.
func TestFleetTask_QuorumUnreachable(t *testing.T) {
	e := newFTEnv(t, quorumDoomed("ft-quorum-doomed", fleetv1.FailurePolicyFailFast),
		childAction("ft-quorum-doomed", "a", fleetv1.ActionPhaseFailed),
		childAction("ft-quorum-doomed", "b", fleetv1.ActionPhaseFailed),
		childAction("ft-quorum-doomed", "c", fleetv1.ActionPhaseInProgress),
	)
	e.reconcile("ft-quorum-doomed")

	ft := e.task("ft-quorum-doomed")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed: quorum 2 with 2 of 3 members failed can never be met, "+
			"and a task that cannot succeed must not sit Running", ft.Status.Phase)
	}
	if got := ft.Status.ActionSummary; got != "0/3 Succeeded" {
		t.Errorf("actionSummary = %q, want \"0/3 Succeeded\"", got)
	}
	c := e.action(childName("ft-quorum-doomed", "c"))
	if c == nil {
		t.Fatal("in-flight member 'c' disappeared")
	}
	if c.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Fatalf("in-flight member 'c' desiredState = %q, want Cancelled — failing the task without "+
			"stopping its running member abandons a robot mid-action", c.Spec.DesiredState)
	}
	if ft.Status.CompletionTime == nil {
		t.Error("no completionTime on a terminal task")
	}
}

// ContinueOthers on the same doomed fixture: the independents are left to finish
// rather than being cancelled, and the task stays Running. The policy appeared in
// three fixtures and had never executed — all three returned before the arm.
func TestFleetTask_ContinueOthersKeepsRunning(t *testing.T) {
	e := newFTEnv(t, quorumDoomed("ft-continue", fleetv1.FailurePolicyContinueOthers),
		childAction("ft-continue", "a", fleetv1.ActionPhaseFailed),
		childAction("ft-continue", "b", fleetv1.ActionPhaseFailed),
		childAction("ft-continue", "c", fleetv1.ActionPhaseInProgress),
	)
	e.reconcile("ft-continue")

	ft := e.task("ft-continue")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running: ContinueOthers lets independents finish rather than "+
			"failing the moment the policy is lost", ft.Status.Phase)
	}
	c := e.action(childName("ft-continue", "c"))
	if c == nil {
		t.Fatal("in-flight member 'c' disappeared")
	}
	if c.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		t.Fatal("in-flight member 'c' was cancelled under ContinueOthers — that is FailFast " +
			"behaviour, and makes the two policies indistinguishable")
	}
	if ft.Status.CompletionTime != nil {
		t.Error("completionTime set on a task that is still Running")
	}

	// And it does not stay Running forever: once the last independent is terminal
	// the task fails, so ContinueOthers defers the failure rather than dropping it.
	e.setPhase(childName("ft-continue", "c"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-continue")
	if got := e.task("ft-continue").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed once nothing is left running: 1 success against a quorum "+
			"of 2 is still short", got)
	}
}

// ── reconcile idempotency ────────────────────────────────────────────────────

// The "Test layers" section of docs/testing.md makes this a standing requirement —
// "Assert idempotency: reconciling a stable object twice produces no status write" —
// and no FleetTask test read a resourceVersion or counted a patch. patchStatus calls
// Status().Patch unconditionally, so the claim rests entirely on the MergeFrom
// diff being empty; that is exactly the thing worth asserting rather than assuming.
//
// resourceVersion is the observable outcome and only means anything against a real
// API server, which is why this test cannot live on the fake client.
//
// KNOWN BLIND SPOT. The passes run back to back, so a status value with only
// one-second resolution — a metav1.Time stamped every reconcile — compares equal
// across them and slips through; that was verified, not assumed (a mutation
// stamping status.startedAt in patchStatus survives this test, one stamping a
// nanosecond value into actionSummary does not). Closing it needs a clock seam in
// the reconciler, not a sleep here: docs/testing.md forbids depending on
// wall-clock timing beyond controlled fakes.
func TestFleetTask_ReconcileIsIdempotent(t *testing.T) {
	task := fleetTask("ft-idem", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task)

	// Settle: pass 1 generates 'a' and writes the status projection; pass 2 is the
	// first pass that should have nothing to do.
	e.reconcile("ft-idem")
	e.setPhase(childName("ft-idem", "a"), fleetv1.ActionPhaseInProgress)
	e.reconcile("ft-idem")
	e.reconcile("ft-idem")

	before := e.task("ft-idem")
	childBefore := e.action(childName("ft-idem", "a"))
	if before.Status.Phase != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("fixture is not in the intended stable state: phase = %q, want Running", before.Status.Phase)
	}

	for i := 0; i < 3; i++ {
		e.reconcile("ft-idem")
	}

	after := e.task("ft-idem")
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("three reconciles of a stable FleetTask changed resourceVersion %s -> %s: the "+
			"reconciler writes status on every pass, which is an etcd write per periodic resync "+
			"(RA-1, docs/coding-standards.md 'Write status only on a material change')",
			before.ResourceVersion, after.ResourceVersion)
	}
	// The child clause is a guard rather than a proven assertion, and it is worth
	// knowing which: removing the fan-out's `if child.Spec.DesiredState != fanoutState`
	// check does NOT make it fail, because the resulting MergeFrom patch is empty
	// and an empty patch is a server-side no-op. It catches the classes that do
	// write — an Update carrying drift, a label or annotation stamped per pass.
	childAfter := e.action(childName("ft-idem", "a"))
	if childAfter.ResourceVersion != childBefore.ResourceVersion {
		t.Errorf("three reconciles re-patched child %q: resourceVersion %s -> %s — the desiredState "+
			"fan-out must be a no-op when the child already carries the task's desiredState",
			childBefore.Name, childBefore.ResourceVersion, childAfter.ResourceVersion)
	}
	// Idempotent must not mean inert: the projection is still there.
	if len(after.Status.Actions) != 2 || after.Status.ActionSummary != "0/2 Succeeded" {
		t.Errorf("status projection was lost across the repeat passes: %d actions, summary %q",
			len(after.Status.Actions), after.Status.ActionSummary)
	}
	// The same must hold once the task is terminal, which takes the early-return
	// path instead of the projection path.
	e.setPhase(childName("ft-idem", "a"), fleetv1.ActionPhaseFailed)
	e.reconcile("ft-idem")
	terminal := e.task("ft-idem")
	if terminal.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed", terminal.Status.Phase)
	}
	e.reconcile("ft-idem")
	e.reconcile("ft-idem")
	if got := e.task("ft-idem"); got.ResourceVersion != terminal.ResourceVersion {
		t.Errorf("reconciling a terminal FleetTask twice changed resourceVersion %s -> %s",
			terminal.ResourceVersion, got.ResourceVersion)
	}
	if terminal.Status.CompletionTime != nil &&
		!terminal.Status.CompletionTime.Equal(e.task("ft-idem").Status.CompletionTime) {
		t.Error("completionTime was rewritten on a later pass; a terminal timestamp must be set once")
	}
}

// ── two shapes the branch tests above leave out ──────────────────────────────
//
// The 2026-08-08 reconciler audit asked for four behaviours to be covered by tests
// that fail if the behaviour is removed. Two shapes it named are not reached by any
// of the branch tests above: a diamond dependency, and quorum's boundary values
// including the single-action degenerate case.

// A diamond: a → {b, c} → d. The join must wait for BOTH predecessors, which a
// dependency check that stops at the first satisfied edge would not do — and every
// prior fixture in the suite had at most one dependsOn per member, so nothing
// distinguished "all predecessors" from "some predecessor".
func TestFleetTask_DiamondJoinWaitsForBothBranches(t *testing.T) {
	task := fleetTask("ft-diamond", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
		ftAction("c", []string{"a"}, fleetv1.ActionPhaseSucceeded),
		ftAction("d", []string{"b", "c"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task)

	e.reconcile("ft-diamond")
	if e.action(childName("ft-diamond", "b")) != nil || e.action(childName("ft-diamond", "c")) != nil {
		t.Fatal("the branches were generated before the fork 'a' succeeded")
	}

	// The fork succeeds: both branches fan out in the same pass.
	e.setPhase(childName("ft-diamond", "a"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-diamond")
	for _, n := range []string{"b", "c"} {
		if e.action(childName("ft-diamond", n)) == nil {
			t.Fatalf("branch %q was not generated once the fork succeeded — a diamond's branches are "+
				"independent and both become eligible at once", n)
		}
	}
	if e.action(childName("ft-diamond", "d")) != nil {
		t.Fatal("the join 'd' was generated while both branches were still unstarted")
	}

	// One branch done is not enough. This is the assertion the suite lacked.
	e.setPhase(childName("ft-diamond", "b"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-diamond")
	if e.action(childName("ft-diamond", "d")) != nil {
		t.Fatal("the join 'd' started with only one of its two predecessors Succeeded — a join that " +
			"waits for the first edge instead of all of them dispatches work whose inputs are not ready")
	}

	e.setPhase(childName("ft-diamond", "c"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-diamond")
	if e.action(childName("ft-diamond", "d")) == nil {
		t.Fatal("the join 'd' was not generated after both branches succeeded")
	}

	e.setPhase(childName("ft-diamond", "d"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("ft-diamond")
	if got := e.task("ft-diamond").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got)
	}
}

// A failed branch takes the join with it: 'd' can never become eligible, so the
// task must resolve rather than wait on a member that will never be created.
func TestFleetTask_DiamondFailedBranchDoesNotStrandTheJoin(t *testing.T) {
	task := fleetTask("ft-diamond-fail", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
		ftAction("c", []string{"a"}, fleetv1.ActionPhaseSucceeded),
		ftAction("d", []string{"b", "c"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task,
		childAction("ft-diamond-fail", "a", fleetv1.ActionPhaseSucceeded),
		childAction("ft-diamond-fail", "b", fleetv1.ActionPhaseSucceeded),
		childAction("ft-diamond-fail", "c", fleetv1.ActionPhaseFailed),
	)
	e.reconcile("ft-diamond-fail")

	if e.action(childName("ft-diamond-fail", "d")) != nil {
		t.Fatal("the join was generated although one branch had failed")
	}
	if got := e.task("ft-diamond-fail").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed: 'd' is permanently ineligible, so an All task that keeps "+
			"waiting for it never terminates", got)
	}
}

// Quorum's boundaries, including the degenerate single-action case and a threshold
// larger than the membership. Every prior quorum fixture used 2-of-3.
func TestFleetTask_QuorumBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		quorum    int32
		phases    []fleetv1.ActionPhase // one per member, in order a, b, c...
		wantPhase fleetv1.FleetTaskPhase
		why       string
	}{
		{"one-of-three-met", 1,
			[]fleetv1.ActionPhase{fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseInProgress},
			fleetv1.FleetTaskPhaseSucceeded, "quorum 1 is Any spelled as a number"},
		{"all-of-three-met", 3,
			[]fleetv1.ActionPhase{fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseSucceeded},
			fleetv1.FleetTaskPhaseSucceeded, "quorum == membership is All spelled as a number"},
		{"all-of-three-one-short", 3,
			[]fleetv1.ActionPhase{fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseInProgress},
			fleetv1.FleetTaskPhaseRunning, "one short with work in flight is not yet decided either way"},
		{"single-action-degenerate", 1,
			[]fleetv1.ActionPhase{fleetv1.ActionPhaseSucceeded},
			fleetv1.FleetTaskPhaseSucceeded, "a one-member quorum of 1"},
		// Nothing in api/v1 bounds spec.quorum above (only Minimum=1), so a
		// threshold above the membership is admissible and unsatisfiable. It must
		// fail rather than sit Running forever.
		{"threshold-above-membership", 5,
			[]fleetv1.ActionPhase{fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseSucceeded},
			fleetv1.FleetTaskPhaseFailed, "a quorum larger than the membership can never be met"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := []string{"a", "b", "c", "d", "e"}[:len(tc.phases)]
			actions := make([]fleetv1.FleetTaskAction, 0, len(names))
			for _, n := range names {
				actions = append(actions, ftAction(n, nil, fleetv1.ActionPhaseSucceeded))
			}
			q := tc.quorum
			task := fleetTask("ft-qb", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyFailFast,
				fleetv1.DesiredStateRunning, actions...)
			task.Spec.Quorum = &q

			objs := []client.Object{task}
			for i, n := range names {
				objs = append(objs, childAction("ft-qb", n, tc.phases[i]))
			}
			e := newFTEnv(t, objs...)
			e.reconcile("ft-qb")

			if got := e.task("ft-qb").Status.Phase; got != tc.wantPhase {
				t.Fatalf("phase = %q, want %q — %s", got, tc.wantPhase, tc.why)
			}
		})
	}
}
