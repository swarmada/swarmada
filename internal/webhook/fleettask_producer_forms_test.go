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

// S6 and S6b across EVERY producer form, not only select.subject.
//
// The source-exists check once ran over a walk that visited select subjects and
// nothing else, so loop.until and fromMember — which name an event type the same
// way and are read at runtime the same way — were unchecked by construction. The
// select-only tests in fleettask_bindings_test.go passed throughout, because
// every one of them built a select. These tests are table-driven over
// allProducerForms so that cannot recur quietly: a form without a fixture fails
// TestProducerForms_EveryFormHasFixtures, and a new route to an event-naming
// field in the API fails TestProducerForms_TypeGraphRoutesArePinned.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Every fixture binds producer member "watch" (external, on ComputeEndpoint
// "watcher") in one form. The flow, where there is one, runs robot member
// "patrol"; "watch" sits beside it as a resident producer, the design case for
// the event arm.
const (
	fxProducer = "watch"
	fxEndpoint = "watcher"
)

// producerFormFixture builds, for one form, a task whose binding reads fxProducer
// by event and by result, and names the field path an event-type rejection must
// be reported at.
type producerFormFixture struct {
	byEvent   func(eventType string) *fleetv1.FleetTask
	byResult  func() *fleetv1.FleetTask
	eventPath string
}

func untilFlow(c fleetv1.UntilCondition) *fleetv1.FlowNode {
	return &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode:  fleetv1.LoopModeForever,
		Body:  runNode("patrol"),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{c}},
	}}
}

// fromMemberTask builds a task whose member "inspect" takes parameter "zone"
// from fxProducer. The consumer's own endpoint is never read by this check.
func fromMemberTask(b fleetv1.MemberBinding) *fleetv1.FleetTask {
	b.Name = fxProducer
	consumer := externalMember("inspect", "inspector")
	consumer.Action.Executor.Parameters = map[string]fleetv1.ParameterBinding{"zone": {FromMember: &b}}
	return ftWith("t1", externalMember(fxProducer, fxEndpoint), consumer)
}

var producerFormFixtures = map[*producerForm]producerFormFixture{
	formSelectSubject: {
		byEvent: func(ev string) *fleetv1.FleetTask {
			return ftWithFlow("t1", selectFlow(eventSubject(ev, fxProducer, "zone")),
				externalMember(fxProducer, fxEndpoint))
		},
		byResult: func() *fleetv1.FleetTask {
			return ftWithFlow("t1", selectFlow(resultSubject(fxProducer, "zone")),
				externalMember(fxProducer, fxEndpoint))
		},
		eventPath: "spec.flow.select.subject.event",
	},
	formLoopUntil: {
		byEvent: func(ev string) *fleetv1.FleetTask {
			return ftWithFlow("t1",
				untilFlow(fleetv1.UntilCondition{Event: &fleetv1.UntilEventCondition{Type: ev, From: fxProducer}}),
				ftAct("patrol"), externalMember(fxProducer, fxEndpoint))
		},
		byResult: func() *fleetv1.FleetTask {
			return ftWithFlow("t1",
				untilFlow(fleetv1.UntilCondition{Result: &fleetv1.UntilResultCondition{
					Member: fxProducer, Field: "zone", Equals: "dock"}}),
				ftAct("patrol"), externalMember(fxProducer, fxEndpoint))
		},
		eventPath: "spec.flow.loop.until.anyOf[0].event.type",
	},
	formFromMember: {
		byEvent: func(ev string) *fleetv1.FleetTask {
			f := "zone"
			return fromMemberTask(fleetv1.MemberBinding{Event: &ev, Field: &f})
		},
		byResult: func() *fleetv1.FleetTask {
			r := "zone"
			return fromMemberTask(fleetv1.MemberBinding{Result: &r})
		},
		eventPath: "spec.actions[1].action.executor.parameters[zone].fromMember.event",
	},
}

// forEachForm runs fn as a subtest per producer form, in allProducerForms order.
func forEachForm(t *testing.T, fn func(t *testing.T, f *producerForm, fx producerFormFixture)) {
	t.Helper()
	for _, f := range allProducerForms {
		fx, ok := producerFormFixtures[f]
		if !ok {
			t.Fatalf("producer form %q has no fixture; see TestProducerForms_EveryFormHasFixtures", f.name)
		}
		t.Run(f.name, func(t *testing.T) { fn(t, f, fx) })
	}
}

// ── S6b — the named event type must be declared, in every form ──

// REJECTS a typo, the case that went unrefused for until and fromMember: one
// character off a declared type.
// For loop.until this is the silent one — an unsatisfied until is not an error,
// so without this rejection the loop runs to its bound and nothing says why.
func TestProducerForms_UndeclaredEventTypeRejected(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected", "NoDetection"))
		_, err := v.ValidateCreate(context.Background(), fx.byEvent("IntrudrDetected"))
		if err == nil {
			t.Fatalf("%s bound to an event type the producer does not declare must be rejected", f.name)
		}
		msg := err.Error()
		for _, want := range []string{
			reasonUndeclaredEvent, "IntrudrDetected", "IntruderDetected", "NoDetection",
			"emitsEvents", f.name, fx.eventPath, f.neverRaised,
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message %q does not contain %q", msg, want)
			}
		}
	})
}

// REJECTS on an EMPTY declaration, as the select case does: a readable Service
// endpoint declaring no events is a definite answer, not a missing one.
func TestProducerForms_EndpointDeclaringNoEventsRejectsAnyEventBinding(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService))
		_, err := v.ValidateCreate(context.Background(), fx.byEvent("IntruderDetected"))
		if err == nil {
			t.Fatalf("%s: a Service endpoint declaring no events must reject an event binding", f.name)
		}
		if !strings.Contains(err.Error(), "none declared") {
			t.Fatalf("message %q must say the endpoint declares nothing", err.Error())
		}
	})
}

// ADMITS the declared type, so the rejections above cannot be satisfied by a
// check that refuses every event binding.
func TestProducerForms_DeclaredEventTypeAdmitted(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
		if _, err := v.ValidateCreate(context.Background(), fx.byEvent("IntruderDetected")); err != nil {
			t.Fatalf("%s naming a declared event type must be admitted: %v", f.name, err)
		}
	})
}

// FAILS OPEN on a read error, as decided at validateProducerContracts, with the
// same control as the select test: the SAME endpoint, readable, rejects the
// SAME task — so this cannot pass because the fixture was admissible anyway.
func TestProducerForms_UndeclaredEventFailsOpenWhenTheEndpointCannotBeRead(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		ep := endpoint(fxEndpoint, fleetv1.ExecutorContractService, "NoDetection")
		task := fx.byEvent("IntruderDetected")

		if _, err := validatorWith(t, ep).ValidateCreate(context.Background(), task); err == nil {
			t.Fatalf("control failed for %s: the fixture must be rejectable when the endpoint IS readable", f.name)
		}

		c := fake.NewClientBuilder().WithScheme(bindScheme(t)).WithObjects(ep).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, isEndpoint := obj.(*fleetv1.ComputeEndpoint); isEndpoint {
						return errors.New("etcd unavailable")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		if _, err := (&FleetTaskValidator{Reader: c}).ValidateCreate(context.Background(), task); err != nil {
			t.Fatalf("%s: an unreadable dependency must not block an otherwise valid write; got %v", f.name, err)
		}
	})
}

// ── S6 — the binding kind must not contradict the producer's contract ──

// An event binding onto an APP producer gets the CONTRACT message, not the
// membership one. The membership message would tell the author to add the type
// to an App endpoint's emitsEvents, which is the wrong fix: an App raises no
// events, and the right binding is to its result, spelled for this form.
func TestProducerForms_EventBindingOntoAnAppProducerNamesTheContract(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractApp))
		_, err := v.ValidateCreate(context.Background(), fx.byEvent("IntruderDetected"))
		if err == nil {
			t.Fatalf("%s: an event binding onto an App producer must be rejected", f.name)
		}
		msg := err.Error()
		if !strings.Contains(msg, reasonContractMismatch) || !strings.Contains(msg, "contract: App") {
			t.Fatalf("message %q does not name the producer's contract", msg)
		}
		if strings.Contains(msg, reasonUndeclaredEvent) {
			t.Fatalf("message %q gives the emitsEvents fix for an App producer, which raises no events", msg)
		}
		if !strings.Contains(msg, f.resultHint(fxProducer)) {
			t.Fatalf("message %q does not spell the result form of %s", msg, f.name)
		}
	})
}

func TestProducerForms_ResultBindingOntoAServiceProducerRejected(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
		_, err := v.ValidateCreate(context.Background(), fx.byResult())
		if err == nil {
			t.Fatalf("%s: a result binding onto a Service producer must be rejected (check S6)", f.name)
		}
		msg := err.Error()
		for _, want := range []string{reasonContractMismatch, "contract: Service", "IntruderDetected", f.eventHint(fxProducer)} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message %q does not contain %q", msg, want)
			}
		}
	})
}

func TestProducerForms_ResultBindingOntoAnAppProducerAdmitted(t *testing.T) {
	forEachForm(t, func(t *testing.T, f *producerForm, fx producerFormFixture) {
		v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractApp))
		if _, err := v.ValidateCreate(context.Background(), fx.byResult()); err != nil {
			t.Fatalf("%s: a result binding onto an App producer must be admitted: %v", f.name, err)
		}
	})
}

// A fromMember naming only the member, or its artifact arm, reads neither a
// result nor an event: S6 has nothing to contradict and must not treat it as a
// result binding onto a Service.
func TestProducerForms_FromMemberArtifactIsNotSubjectToS6(t *testing.T) {
	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	a := "clip"
	if _, err := v.ValidateCreate(context.Background(), fromMemberTask(fleetv1.MemberBinding{Artifact: &a})); err != nil {
		t.Fatalf("a fromMember artifact binding must not be refused by S6: %v", err)
	}
}

// A compensation carries a whole FleetActionSpec, so a fromMember binding can sit
// on its executor. This is the route the type-graph guard found on its first
// run: producerRefs as first written walked spec.actions[].action only.
func TestProducerForms_CompensationFromMemberIsChecked(t *testing.T) {
	ev, f := "IntrudrDetected", "zone"
	comp := externalMember("undo", "inspector").Action
	comp.Executor.Parameters = map[string]fleetv1.ParameterBinding{
		"zone": {FromMember: &fleetv1.MemberBinding{Name: fxProducer, Event: &ev, Field: &f}},
	}
	robot := ftAct("patrol")
	robot.Compensation = &comp
	task := ftWith("t1", externalMember(fxProducer, fxEndpoint), robot)

	v := validatorWith(t, endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected"))
	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a compensation's fromMember bound to an undeclared event type must be rejected")
	}
	if want := "spec.actions[1].compensation.executor.parameters[zone].fromMember.event"; !strings.Contains(err.Error(), want) {
		t.Fatalf("message %q is not reported at %s", err.Error(), want)
	}

	ev = "IntruderDetected"
	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("a compensation's fromMember naming a declared event type must be admitted: %v", err)
	}
}

// ONE READ PER ENDPOINT. Three references to the same producer are one
// question to the API server, not three.
func TestProducerForms_EndpointIsReadOncePerAdmission(t *testing.T) {
	gets := 0
	c := fake.NewClientBuilder().WithScheme(bindScheme(t)).
		WithObjects(endpoint(fxEndpoint, fleetv1.ExecutorContractService, "IntruderDetected")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isEndpoint := obj.(*fleetv1.ComputeEndpoint); isEndpoint {
					gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	task := producerFormFixtures[formFromMember].byEvent("IntruderDetected")
	task.Spec.Flow = &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Body: fleetv1.FlowNode{Select: &fleetv1.SelectNode{
			Subject: eventSubject("IntruderDetected", fxProducer, "zone"),
			Default: runNode("inspect"),
		}},
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Event: &fleetv1.UntilEventCondition{Type: "IntruderDetected", From: fxProducer}},
		}},
	}}
	if _, err := (&FleetTaskValidator{Reader: c}).ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("fixture must be admissible: %v", err)
	}
	if gets != 1 {
		t.Fatalf("three references to one producer read its ComputeEndpoint %d times, want 1", gets)
	}
}

// ── The guards: a fourth form cannot arrive unchecked ──

// Every form producerRefs can emit has fixtures, so every test above covers it.
func TestProducerForms_EveryFormHasFixtures(t *testing.T) {
	if len(producerFormFixtures) != len(allProducerForms) {
		t.Fatalf("%d producer forms, %d fixtures: every form in allProducerForms needs one, and "+
			"every fixture must be for a form producerRefs emits", len(allProducerForms), len(producerFormFixtures))
	}
	for _, f := range allProducerForms {
		if _, ok := producerFormFixtures[f]; !ok {
			t.Fatalf("producer form %q has no fixture", f.name)
		}
	}
}

// pinnedEventRoutes is every struct field reachable from FleetTaskSpec that
// either names an event type itself (its name contains "Event") or leads to one.
// It is the API-side answer to "where can a FleetTask name an event", computed
// from the Go types, and producerRefs is the webhook-side answer. The leaves map
// to the producerForm that covers them.
//
// Pinned rather than derived because the point is to FAIL when it changes. A
// new leaf is a new binding form; a new route to an existing leaf is either a
// new container of an existing binding (a second parameter map) or a new flow
// node that can hide a select or a loop from flowProducerRefs' walk. Each needs
// producerRefs extended BEFORE this list is, which is the order this test
// enforces.
var pinnedEventRoutes = map[string]*producerForm{
	// Leaves — the three forms.
	"SelectSubject.Event":  formSelectSubject,
	"UntilCondition.Event": formLoopUntil,
	"MemberBinding.Event":  formFromMember,
	// Routes — each must be walked by producerRefs / flowProducerRefs.
	"FleetTaskSpec.Actions":        nil,
	"FleetTaskAction.Action":       nil,
	"FleetTaskAction.Compensation": nil, // walked by producerRefs; see TestProducerForms_CompensationFromMemberIsChecked
	"FleetActionSpec.Executor":     nil,
	"ActionExecutor.Parameters":    nil,
	"ParameterBinding.FromMember":  nil,
	"FleetTaskSpec.Flow":           nil,
	"FlowNode.Sequence":            nil,
	"FlowNode.Parallel":            nil,
	"ParallelNode.Branches":        nil,
	"FlowNode.Loop":                nil,
	"LoopNode.Body":                nil,
	"LoopNode.Until":               nil,
	"UntilSpec.AnyOf":              nil,
	"FlowNode.Select":              nil,
	"SelectNode.Subject":           nil,
	"SelectNode.Cases":             nil,
	"SelectCase.Then":              nil,
	"SelectNode.Default":           nil,
}

func TestProducerForms_TypeGraphRoutesArePinned(t *testing.T) {
	got := eventRoutes(reflect.TypeOf(fleetv1.FleetTaskSpec{}))

	var added, removed []string
	for r := range got {
		if _, ok := pinnedEventRoutes[r]; !ok {
			added = append(added, r)
		}
	}
	for r := range pinnedEventRoutes {
		if !got[r] {
			removed = append(removed, r)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if len(added)+len(removed) > 0 {
		t.Fatalf("the routes from FleetTaskSpec to a field naming an event type changed "+
			"(new: %v, gone: %v). A new leaf is a new binding form; a new route is a new place a "+
			"binding can sit. Extend producerRefs / flowProducerRefs (fleettask_bindings.go) to "+
			"visit it, add its form to allProducerForms and producerFormFixtures, and only then "+
			"update pinnedEventRoutes — otherwise the emitsEvents check covers less than its "+
			"message says", added, removed)
	}

	covered := make(map[*producerForm]bool)
	for _, f := range pinnedEventRoutes {
		if f != nil {
			covered[f] = true
		}
	}
	for _, f := range allProducerForms {
		if !covered[f] {
			t.Fatalf("producer form %q is covered by no pinned leaf", f.name)
		}
	}
}

// eventRoutes returns "Type.Field" for every field of an api/v1 struct reachable
// from root that names an event type or leads to one that does. FlowNode is
// recursive, so reachability is a fixed point rather than a plain DFS.
func eventRoutes(root reflect.Type) map[string]bool {
	pkg := reflect.TypeOf(fleetv1.FleetTaskSpec{}).PkgPath()
	elem := func(t reflect.Type) reflect.Type {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || t.PkgPath() != pkg {
			return nil
		}
		return t
	}

	// Every api/v1 struct reachable from root.
	var types []reflect.Type
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(t reflect.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		types = append(types, t)
		for i := 0; i < t.NumField(); i++ {
			visit(elem(t.Field(i).Type))
		}
	}
	visit(elem(root))

	isLeaf := func(f reflect.StructField) bool { return strings.Contains(f.Name, "Event") }
	reaches := map[reflect.Type]bool{}
	for changed := true; changed; {
		changed = false
		for _, t := range types {
			if reaches[t] {
				continue
			}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if isLeaf(f) || reaches[elem(f.Type)] {
					reaches[t], changed = true, true
					break
				}
			}
		}
	}

	out := map[string]bool{}
	for _, t := range types {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if isLeaf(f) || reaches[elem(f.Type)] {
				out[fmt.Sprintf("%s.%s", t.Name(), f.Name)] = true
			}
		}
	}
	return out
}
