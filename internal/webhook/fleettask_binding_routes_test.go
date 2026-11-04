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

// EVERY ROUTE TO AN EXECUTOR, DERIVED AND EXERCISED — not pinned.
//
// TestProducerForms_TypeGraphRoutesArePinned pins the routes to a field that
// names an EVENT, and each pinned route carries a comment saying who walks it.
// Two things escaped it. fromTaskInput names no event, so the compensation
// route was walked for fromMember and not for S1; and a comment
// saying a route is walked is not a test that it is, so the eviction warning
// could cover one form of four while every route stayed pinned.
//
// These tests derive every chain of fields from FleetTaskAction to an
// ActionExecutor from the Go types, BUILD a task with a binding at the end of
// each chain, and assert what admission says about it. A new route — a second
// FleetActionSpec on a member, an inline action on a flow node — is exercised
// the moment it compiles, with no list to update first.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// routeElem unwraps pointers, slices, arrays and maps to the api/v1 struct they
// hold, or nil when the field leads to no api/v1 struct.
func routeElem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != reflect.TypeOf(fleetv1.FleetTaskSpec{}).PkgPath() {
		return nil
	}
	return t
}

// executorChains returns every chain of struct fields from root that ends at a
// field holding an ActionExecutor. FlowNode is recursive, so a type already on
// the current chain is not re-entered: a route through a second level of the
// same node is the same route.
func executorChains(root reflect.Type) [][]reflect.StructField {
	target := reflect.TypeOf(fleetv1.ActionExecutor{})
	var out [][]reflect.StructField
	onChain := map[reflect.Type]bool{}
	var walk func(t reflect.Type, prefix []reflect.StructField)
	walk = func(t reflect.Type, prefix []reflect.StructField) {
		onChain[t] = true
		defer delete(onChain, t)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			et := routeElem(f.Type)
			if et == nil {
				continue
			}
			chain := append(append([]reflect.StructField(nil), prefix...), f)
			if et == target {
				out = append(out, chain)
				continue
			}
			if !onChain[et] {
				walk(et, chain)
			}
		}
	}
	walk(root, nil)
	return out
}

func chainName(chain []reflect.StructField) string {
	names := make([]string, len(chain))
	for i, f := range chain {
		names[i] = f.Name
	}
	return strings.Join(names, ".")
}

// chainPath is the admission field path of the executor at the end of chain,
// below base, spelled from the json tags — so it is the API's own spelling and
// not the webhook's.
func chainPath(base *field.Path, chain []reflect.StructField) *field.Path {
	p := base
	for _, f := range chain {
		p = p.Child(strings.Split(f.Tag.Get("json"), ",")[0])
		for t := f.Type; t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice; t = t.Elem() {
			if t.Kind() == reflect.Slice {
				p = p.Index(0)
			}
		}
	}
	return p
}

// setAlong allocates every pointer and one element of every slice along chain,
// starting at the addressable struct v, and puts ex at its end.
func setAlong(v reflect.Value, chain []reflect.StructField, ex *fleetv1.ActionExecutor) error {
	for i, f := range chain {
		fv := v.FieldByIndex(f.Index)
		if i == len(chain)-1 {
			switch fv.Type() {
			case reflect.TypeOf(ex):
				fv.Set(reflect.ValueOf(ex))
			case reflect.TypeOf(*ex):
				fv.Set(reflect.ValueOf(*ex))
			default:
				return fmt.Errorf("%s holds %s; extend setAlong to build it", chainName(chain), fv.Type())
			}
			return nil
		}
		for fv.Kind() != reflect.Struct {
			switch fv.Kind() {
			case reflect.Pointer:
				if fv.IsNil() {
					fv.Set(reflect.New(fv.Type().Elem()))
				}
				fv = fv.Elem()
			case reflect.Slice:
				if fv.Len() == 0 {
					fv.Set(reflect.MakeSlice(fv.Type(), 1, 1))
				}
				fv = fv.Index(0)
			default:
				return fmt.Errorf("%s passes through a %s; extend setAlong to build it", chainName(chain), fv.Kind())
			}
		}
		v = fv
	}
	return nil
}

// memberRoutes is every executor route on one member, each with a robot member
// "patrol" carrying ex at the end of it, and the path admission must report at.
type memberRoute struct {
	name   string
	member fleetv1.FleetTaskAction
	path   *field.Path
}

func memberRoutes(t *testing.T, index int, ex func() *fleetv1.ActionExecutor) []memberRoute {
	t.Helper()
	chains := executorChains(reflect.TypeOf(fleetv1.FleetTaskAction{}))
	// Two today: the member's action and its compensation. Fewer means the
	// derivation broke, and every test below would pass vacuously.
	if len(chains) < 2 {
		t.Fatalf("derived %d executor routes on a FleetTaskAction, want at least action and compensation", len(chains))
	}
	var out []memberRoute
	for _, c := range chains {
		m := ftAct("patrol")
		if err := setAlong(reflect.ValueOf(&m).Elem(), c, ex()); err != nil {
			t.Fatal(err)
		}
		out = append(out, memberRoute{
			name:   chainName(c),
			member: m,
			path:   chainPath(field.NewPath("spec", "actions").Index(index), c),
		})
	}
	return out
}

// A FleetTask names an executor only on a member. A route that does not start
// at spec.actions — a flow node carrying an inline action — is a container the
// per-member walk cannot see, and the tests below build members only.
func TestBindingRoutes_ExecutorsSitOnlyOnMembers(t *testing.T) {
	for _, c := range executorChains(reflect.TypeOf(fleetv1.FleetTaskSpec{})) {
		if c[0].Name != "Actions" {
			t.Errorf("FleetTaskSpec reaches an ActionExecutor through %s, not through spec.actions. "+
				"taskExecutors (fleettask_bindings.go) must walk it, and memberRoutes must build it",
				chainName(c))
		}
	}
}

// S1 ON EVERY ROUTE. A fromTaskInput naming an undeclared input is rejected
// wherever the executor sits.
func TestBindingRoutes_FromTaskInputIsCheckedOnEveryExecutorRoute(t *testing.T) {
	bind := func(input string) func() *fleetv1.ActionExecutor {
		return func() *fleetv1.ActionExecutor {
			return &fleetv1.ActionExecutor{EndpointRef: "undo-svc", Parameters: map[string]fleetv1.ParameterBinding{
				"region": {FromTaskInput: &input},
			}}
		}
	}
	inputs := []fleetv1.TaskInput{{Name: "region", Kind: fleetv1.ValueKindString}}

	for _, r := range memberRoutes(t, 0, bind("regoin")) {
		t.Run(r.name, func(t *testing.T) {
			task := ftWith("t1", r.member)
			task.Spec.Inputs = inputs
			_, err := validatorWith(t).ValidateCreate(context.Background(), task)
			if err == nil {
				t.Fatalf("a fromTaskInput naming an undeclared input at %s must be rejected", r.path)
			}
			want := r.path.Child("parameters").Key("region").String()
			for _, s := range []string{reasonUndeclaredTaskInput, "regoin", want} {
				if !strings.Contains(err.Error(), s) {
					t.Fatalf("message %q does not contain %q", err.Error(), s)
				}
			}
		})
	}
	for _, r := range memberRoutes(t, 0, bind("region")) {
		t.Run(r.name+"/declared", func(t *testing.T) {
			task := ftWith("t1", r.member)
			task.Spec.Inputs = inputs
			if _, err := validatorWith(t).ValidateCreate(context.Background(), task); err != nil {
				t.Fatalf("a fromTaskInput naming a declared input at %s must be admitted: %v", r.path, err)
			}
		})
	}
}

// S6b AND THE WARNING ON EVERY ROUTE. A fromMember event binding is refused on
// an undeclared type wherever it sits, and an admitted one is always named in a
// warning — the eviction window where the runtime reads it, the missing
// resolution where it does not. A route that is checked and never mentioned is
// shape the select-only warning had.
func TestBindingRoutes_FromMemberIsCheckedAndWarnedOnEveryExecutorRoute(t *testing.T) {
	bind := func(ev string) func() *fleetv1.ActionExecutor {
		return func() *fleetv1.ActionExecutor {
			f := "zone"
			return &fleetv1.ActionExecutor{EndpointRef: "undo-svc", Parameters: map[string]fleetv1.ParameterBinding{
				"zone": {FromMember: &fleetv1.MemberBinding{Name: fxProducer, Event: &ev, Field: &f}},
			}}
		}
	}
	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	task := func(m fleetv1.FleetTaskAction) *fleetv1.FleetTask {
		return ftWithFlow("t1", &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("patrol")}},
			m, externalMember(fxProducer, fxEndpoint))
	}

	for _, r := range memberRoutes(t, 0, bind("IntrudrDetected")) {
		t.Run(r.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), task(r.member))
			if err == nil {
				t.Fatalf("a fromMember naming an undeclared event type at %s must be rejected", r.path)
			}
			want := r.path.Child("parameters").Key("zone").Child("fromMember", "event").String()
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("message %q is not reported at %s", err.Error(), want)
			}
		})
	}
	for _, r := range memberRoutes(t, 0, bind("IntruderDetected")) {
		t.Run(r.name+"/declared", func(t *testing.T) {
			warnings, err := v.ValidateCreate(context.Background(), task(r.member))
			if err != nil {
				t.Fatalf("a fromMember naming a declared event type at %s must be admitted: %v", r.path, err)
			}
			want := r.path.Child("parameters").Key("zone").Child("fromMember", "event").String()
			if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, want) {
				t.Fatalf("admitted event binding at %s is named in no warning: %q", want, joined)
			}
		})
	}
}

// ── The eviction warning covers every form the runtime reads ──

// Every form's event fixture is named in a warning, at its own path.
func TestBindingWarnings_EveryEventFormIsNamed(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
		warnings, err := v.ValidateCreate(context.Background(), fx.byEvent("IntruderDetected"))
		if err != nil {
			t.Fatalf("%s fixture must be admissible: %v", f.name, err)
		}
		if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, fx.eventPath) {
			t.Fatalf("%s event binding at %s is named in no warning: %q", f.name, fx.eventPath, joined)
		}
	})
}

// THE SILENT ONE. A task whose only event binding is a loop.until condition got
// no warning at all, and it is the form that fails most quietly: an evicted
// event is an unsatisfied condition, and the loop runs to its bound.
func TestBindingWarnings_LoopUntilOnlyTaskIsWarned(t *testing.T) {
	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	task := ftWithFlow("t1",
		untilFlow(fleetv1.UntilCondition{Event: &fleetv1.UntilEventCondition{Type: "IntruderDetected", From: fxProducer}}),
		ftAct("patrol"), externalMember(fxProducer, fxEndpoint))

	warnings, err := v.ValidateCreate(context.Background(), task)
	if err != nil {
		t.Fatalf("fixture must be admissible: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{
		"BOUNDED WINDOW", "status.events", "eventsDropped",
		"spec.flow.loop.until.anyOf[0].event.type", "maxIterations",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warning %q does not contain %q", joined, want)
		}
	}
	// The warning names what the task has, not what some other task might: a
	// loop-only task told about a select it does not contain is reading a
	// message written for someone else.
	if strings.Contains(joined, "select") {
		t.Fatalf("warning for a task with no select mentions one: %q", joined)
	}
}

// A fromMember event binding on a member a flow starts is read, once, at the
// child's creation — the window applies to it as to the other two forms.
func TestBindingWarnings_FlowStartedFromMemberGetsTheEvictionWarning(t *testing.T) {
	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	task := producerFormFixtures[formFromMember].byEvent("IntruderDetected")
	task.Spec.Flow = &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("inspect")}}

	warnings, err := v.ValidateCreate(context.Background(), task)
	if err != nil {
		t.Fatalf("fixture must be admissible: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"BOUNDED WINDOW", producerFormFixtures[formFromMember].eventPath} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warning %q does not contain %q", joined, want)
		}
	}
}

// A fromMember the runtime never resolves must not be told about an eviction
// window it is never read through: it is told it is not resolved. Two routes
// today — a compensation (created from the spec as written) and a member of a
// task with no spec.flow (the dependsOn path creates it from the spec as
// written too). Both are admitted and S6b-checked.
func TestBindingWarnings_UnresolvedFromMemberIsToldSo(t *testing.T) {
	ev, f := "IntruderDetected", "zone"
	comp := externalMember("undo", "inspector").Action
	comp.Executor.Parameters = map[string]fleetv1.ParameterBinding{
		"zone": {FromMember: &fleetv1.MemberBinding{Name: fxProducer, Event: &ev, Field: &f}},
	}
	robot := ftAct("patrol")
	robot.Compensation = &comp

	cases := map[string]struct {
		task *fleetv1.FleetTask
		path string
	}{
		"compensation": {
			task: ftWithFlow("t1", &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("patrol")}},
				externalMember(fxProducer, fxEndpoint), robot),
			path: "spec.actions[1].compensation.executor.parameters[zone].fromMember.event",
		},
		"no flow": {
			task: producerFormFixtures[formFromMember].byEvent("IntruderDetected"),
			path: producerFormFixtures[formFromMember].eventPath,
		},
	}
	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			warnings, err := v.ValidateCreate(context.Background(), tc.task)
			if err != nil {
				t.Fatalf("fixture must be admissible: %v", err)
			}
			var hit string
			for _, w := range warnings {
				if strings.Contains(w, tc.path) {
					hit = w
				}
			}
			if hit == "" {
				t.Fatalf("no warning names %s: %q", tc.path, warnings)
			}
			if !strings.Contains(hit, "NOT RESOLVED") || strings.Contains(hit, "BOUNDED WINDOW") {
				t.Fatalf("the warning naming %s must say it is not resolved, and must not describe an "+
					"eviction window it is never read through: %q", tc.path, hit)
			}
		})
	}
}
