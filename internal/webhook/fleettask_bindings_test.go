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

// Tier 1 parameter-provenance admission, at the boundary it is enforced from.
//
// S6 is the load-bearing one: the schema admits exactly the contract-appropriate
// form, and admission rejects the other with a message naming the producer's
// contract. It was decided and documented long before anything enforced it,
// which is the half these tests are.

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func bindScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sc := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sc); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return sc
}

// endpoint builds a ComputeEndpoint declaring a contract, in the task's namespace.
func endpoint(name string, contract fleetv1.ExecutorContract, events ...string) *fleetv1.ComputeEndpoint {
	return &fleetv1.ComputeEndpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.ComputeEndpointSpec{Contract: contract, EmitsEvents: events},
	}
}

// externalMember builds a member whose action is dispatched to a ComputeEndpoint.
func externalMember(name, endpointRef string) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{Name: name, Action: fleetv1.FleetActionSpec{
		Type:     fleetv1.ActionTypeExternal,
		Executor: &fleetv1.ActionExecutor{EndpointRef: endpointRef},
	}}
}

func validatorWith(t *testing.T, objs ...*fleetv1.ComputeEndpoint) *FleetTaskValidator {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(bindScheme(t))
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &FleetTaskValidator{Reader: b.Build()}
}

func resultSubject(member, field string) fleetv1.SelectSubject {
	return fleetv1.SelectSubject{Member: member, Field: field}
}

func eventSubject(event, from, field string) fleetv1.SelectSubject {
	return fleetv1.SelectSubject{Event: &event, From: from, Field: field}
}

func selectFlow(subject fleetv1.SelectSubject) *fleetv1.FlowNode {
	return &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: subject,
		Default: fleetv1.FlowNode{},
	}}
}

// ── S6 — the binding kind must not contradict the producer's contract ──

// THE CRITICAL ONE. A result binding onto a SERVICE producer is the case this
// rule exists to prevent: a resident producer has no terminal result, so the
// branch would read either nothing or something that keeps changing.
func TestSelectContracts_ResultBindingOntoAServiceProducerRejected(t *testing.T) {
	v := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified", "NoDetection"))
	task := ftWithFlow("t1", selectFlow(resultSubject("classify", "label")),
		externalMember("classify", "classifier"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a result binding onto a Service-contract producer must be rejected (check S6)")
	}
	msg := err.Error()
	// The message must name the CONTRACT — that is the whole point of deciding
	// this at admission rather than leaving it to the author.
	if !strings.Contains(msg, "Service") {
		t.Fatalf("message %q does not name the producer's contract", msg)
	}
	// And it must name the events the author could bind to instead; "you chose
	// wrong" without "here is the set" is a message that sends someone to the
	// endpoint object to find out.
	if !strings.Contains(msg, "ObjectClassified") {
		t.Fatalf("message %q does not list the events the producer declares", msg)
	}
}

// The mirror: an event binding onto an APP producer. An App runs to completion
// and raises no events, so the branch would wait on a channel nothing writes to.
func TestSelectContracts_EventBindingOntoAnAppProducerRejected(t *testing.T) {
	v := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp))
	task := ftWithFlow("t1", selectFlow(eventSubject("PlateRead", "plate-read", "plate")),
		externalMember("plate-read", "plate-reader"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("an event binding onto an App-contract producer must be rejected (check S6)")
	}
	if !strings.Contains(err.Error(), "App") {
		t.Fatalf("message %q does not name the producer's contract", err.Error())
	}
}

// Both correct pairings are admitted, so the two rejections above cannot be
// satisfied by a check that refuses every select over an external producer.
func TestSelectContracts_MatchingBindingsAreAdmitted(t *testing.T) {
	v := validatorWith(t,
		endpoint("plate-reader", fleetv1.ExecutorContractApp),
		endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified"))

	appTask := ftWithFlow("t1", selectFlow(resultSubject("plate-read", "plate")),
		externalMember("plate-read", "plate-reader"))
	if _, err := v.ValidateCreate(context.Background(), appTask); err != nil {
		t.Fatalf("a result binding onto an App producer must be admitted: %v", err)
	}

	svcTask := ftWithFlow("t2", selectFlow(eventSubject("ObjectClassified", "classify", "label")),
		externalMember("classify", "classifier"))
	if _, err := v.ValidateCreate(context.Background(), svcTask); err != nil {
		t.Fatalf("an event binding onto a Service producer must be admitted: %v", err)
	}
}

// A ROBOT member has no ComputeEndpoint and no contract to contradict: it
// produces a result and raises no events, so the result form is the only form
// and S6 has nothing to say. It must not be swept up by a check that assumed
// every producer is external.
func TestSelectContracts_RobotProducerIsNotSubjectToS6(t *testing.T) {
	v := validatorWith(t)
	task := ftWithFlow("t1", selectFlow(resultSubject("patrol", "done")), ftAct("patrol"))
	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("a result binding onto a robot member must be admitted: %v", err)
	}
}

// FAILURE DISCIPLINE. A missing ComputeEndpoint is FleetAction's G1 gate to
// report, on the member that carries the reference. Reporting it here too would
// give one mistake two messages from two objects, and — worse — would make a
// FleetTask unwritable for a reason whose fix is on a different resource.
func TestSelectContracts_MissingEndpointIsNotThisGatesToReport(t *testing.T) {
	v := validatorWith(t) // no ComputeEndpoint objects at all
	task := ftWithFlow("t1", selectFlow(resultSubject("classify", "label")),
		externalMember("classify", "does-not-exist"))
	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("an absent ComputeEndpoint is G1's to report, not this check's: %v", err)
	}
}

// A nil Reader disables ONLY the cross-object check. Unlike the append and
// delete guards, S6 is a correctness check over a second object, not a safety
// invariant over this one — a misconfigured Reader must not make every
// FleetTask unwritable.
func TestSelectContracts_NilReaderDisablesOnlyTheCrossObjectCheck(t *testing.T) {
	v := &FleetTaskValidator{}
	ok := ftWithFlow("t1", selectFlow(resultSubject("classify", "label")),
		externalMember("classify", "classifier"))
	if _, err := v.ValidateCreate(context.Background(), ok); err != nil {
		t.Fatalf("a nil Reader must not reject a structurally valid task: %v", err)
	}
	// ...and the structural checks still run.
	bad := ftWithFlow("t2", selectFlow(resultSubject("ghost", "label")), ftAct("patrol"))
	if _, err := v.ValidateCreate(context.Background(), bad); err == nil {
		t.Fatal("a nil Reader must not disable the structural member check")
	}
}

// ── S1 — a binding names something that is not declared ──

// `fromTaskInput` named `spec.inputs` since the field was added, and
// `spec.inputs` did not exist. Now that it does, a binding onto an undeclared
// input is rejected, on create and on an update that changes the bound spec.
func TestBindings_FromTaskInputMustNameADeclaredInput(t *testing.T) {
	v := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp))
	member := externalMember("plate-read", "plate-reader")
	undeclared := "confFloor"
	member.Action.Executor.Parameters = map[string]fleetv1.ParameterBinding{
		"minPlateConfidence": {FromTaskInput: &undeclared},
	}
	task := ftWith("t1", member)
	task.Spec.Inputs = []fleetv1.TaskInput{{Name: "region", Kind: fleetv1.ValueKindString}}

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a fromTaskInput binding naming an undeclared input must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "confFloor") {
		t.Fatalf("message %q does not name the undeclared input", msg)
	}
	// It must say what IS declared: an author whose typo is one character off
	// needs the list, not the word "invalid".
	if !strings.Contains(msg, "region") {
		t.Fatalf("message %q does not list the declared inputs", msg)
	}
}

func TestBindings_FromTaskInputNamingADeclaredInputIsAdmitted(t *testing.T) {
	v := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp))
	member := externalMember("plate-read", "plate-reader")
	declared := "region"
	member.Action.Executor.Parameters = map[string]fleetv1.ParameterBinding{
		"region": {FromTaskInput: &declared},
	}
	task := ftWith("t1", member)
	task.Spec.Inputs = []fleetv1.TaskInput{
		{Name: "region", Kind: fleetv1.ValueKindEnum, Allowed: []string{"US", "EU"}},
	}
	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("a binding onto a declared input must be admitted: %v", err)
	}
}

func TestBindings_FromMemberMustNameARealMember(t *testing.T) {
	v := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp))
	member := externalMember("plate-read", "plate-reader")
	member.Action.Executor.Parameters = map[string]fleetv1.ParameterBinding{
		"plateHint": {FromMember: &fleetv1.MemberBinding{Name: "no-such-member"}},
	}
	_, err := v.ValidateCreate(context.Background(), ftWith("t1", member))
	if err == nil {
		t.Fatal("a fromMember binding naming a member not in spec.actions must be rejected")
	}
	if !strings.Contains(err.Error(), "no-such-member") {
		t.Fatalf("message %q does not name the member that does not exist", err.Error())
	}
}

// ── The event arm is admitted and warned about, not silently accepted ──

// An event-bound select is CORRECT against the design and NOT EVALUABLE at
// v0.4. Rejecting it would make a correctly-written task unwritable; accepting
// it silently would let the author discover it as a node that failed at 3am. So
// it is admitted WITH a warning, at the cheapest possible moment.
func TestSelectContracts_EventBindingIsAdmittedWithARetentionWarning(t *testing.T) {
	v := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified"))
	task := ftWithFlow("t1", selectFlow(eventSubject("ObjectClassified", "classify", "label")),
		externalMember("classify", "classifier"))

	warnings, err := v.ValidateCreate(context.Background(), task)
	if err != nil {
		t.Fatalf("an event binding onto a Service producer must be ADMITTED: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("an event binding must warn about the bounded window it reads; silence sends the " +
			"author to debug a node that waits forever with no clue why")
	}
	joined := strings.Join(warnings, " ")
	// The warning must name the WINDOW and the counter that reports the loss.
	// It must NOT still claim the arm is unevaluable — it is read now, and a
	// warning an author has learned to ignore because it was wrong once is
	// worse than no warning at all.
	if strings.Contains(joined, "NOT EVALUABLE") && strings.Contains(joined, "select.subject binds to a semantic event") {
		t.Fatalf("the event-arm warning still claims the arm is unevaluable: %q", joined)
	}
	for _, want := range []string{"status.events", "spec.flow", "eventsDropped", "BOUNDED WINDOW"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warning %q does not mention %q", joined, want)
		}
	}
}

// A result-bound select warns about nothing. A warning on every task teaches a
// reader to ignore warnings.
func TestSelectContracts_ResultBindingWarnsAboutNothing(t *testing.T) {
	v := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp))
	task := ftWithFlow("t1", selectFlow(resultSubject("plate-read", "plate")),
		externalMember("plate-read", "plate-reader"))
	warnings, err := v.ValidateCreate(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("an evaluable binding must warn about nothing, got %v", warnings)
	}
}

// ── S6b — the named event type must be declared in spec.emitsEvents ──
//
// THE SOURCE-EXISTS CHECK. validateProducerContracts already fetched the
// ComputeEndpoint and already printed its spec.emitsEvents in the S6 message; all
// that was missing was asking whether the named type is in it.
//
// Tested the crossobject way — rejects the invalid case, admits the valid one,
// and behaves as DECIDED when the lookup errors — because a rejection-only test
// would not notice the third. The decision here is OPEN on unknown, CLOSED on
// known, and the reason is stated at the check: a select bound to an undeclared
// event is a correctness failure for its author, not an open safety gate, so the
// cost of refusing correct tasks during an API-server hiccup is the higher one.

// REJECTS: the endpoint is a Service and declares events, but not this one.
func TestSelectContracts_UndeclaredEventTypeRejected(t *testing.T) {
	v := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified", "NoDetection"))
	task := ftWithFlow("t1", selectFlow(eventSubject("IntruderDetected", "classify", "zone")),
		externalMember("classify", "classifier"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a select bound to an event the producer does not declare must be rejected — it is a node that waits forever")
	}
	msg := err.Error()
	if !strings.Contains(msg, "IntruderDetected") {
		t.Fatalf("message %q does not name the undeclared event type", msg)
	}
	// The declared set, for the same reason S6 prints it: an author one
	// character off needs the list, not the word "invalid".
	if !strings.Contains(msg, "ObjectClassified") || !strings.Contains(msg, "NoDetection") {
		t.Fatalf("message %q does not list what the endpoint does declare", msg)
	}
	if !strings.Contains(msg, "emitsEvents") {
		t.Fatalf("message %q does not name the field that would fix it", msg)
	}
}

// REJECTS, and this is the decision rather than the obvious case: an EMPTY
// emitsEvents on a readable Service endpoint is a definite answer — "this
// producer declares no events at all" — and a different fix from choosing the
// wrong one. Admitting it would be inheriting the Get's fail-open disposition
// into a check that has a definite answer in hand.
func TestSelectContracts_EndpointDeclaringNoEventsRejectsAnyEventSubject(t *testing.T) {
	v := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService))
	task := ftWithFlow("t1", selectFlow(eventSubject("ObjectClassified", "classify", "label")),
		externalMember("classify", "classifier"))

	_, err := v.ValidateCreate(context.Background(), task)
	if err == nil {
		t.Fatal("a Service endpoint declaring no events must reject an event subject: the list is a definite answer, not a missing one")
	}
	if !strings.Contains(err.Error(), "none declared") {
		t.Fatalf("message %q must say the endpoint declares nothing, not list an empty set", err.Error())
	}
}

// ADMITS: the declared case, so the two rejections above cannot be satisfied by
// a check that refuses every event subject.
func TestSelectContracts_DeclaredEventTypeAdmitted(t *testing.T) {
	v := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified", "IntruderDetected"))
	task := ftWithFlow("t1", selectFlow(eventSubject("IntruderDetected", "classify", "zone")),
		externalMember("classify", "classifier"))

	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("an event subject naming a declared type must be admitted: %v", err)
	}
}

// ON A READ ERROR IT FAILS OPEN, AS DECIDED. errors.New and not NotFound: a
// NotFound is a definite answer that G1 reports, anything else is "we do not
// know". The endpoint below WOULD reject the subject if it could be read, so
// this test fails if the disposition is ever silently flipped to closed — and it
// is the test that has to be revisited, alongside the comment stating why, if
// the check is ever reused to gate something safety-relevant.
func TestSelectContracts_UndeclaredEventFailsOpenWhenTheEndpointCannotBeRead(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(bindScheme(t)).
		WithObjects(endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isEndpoint := obj.(*fleetv1.ComputeEndpoint); isEndpoint {
					return errors.New("etcd unavailable")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	v := &FleetTaskValidator{Reader: c}

	task := ftWithFlow("t1", selectFlow(eventSubject("IntruderDetected", "classify", "zone")),
		externalMember("classify", "classifier"))

	// The control: with the SAME endpoint readable, this task is rejected.
	if _, err := validatorWith(t, endpoint("classifier", fleetv1.ExecutorContractService, "ObjectClassified")).
		ValidateCreate(context.Background(), task); err == nil {
		t.Fatal("control failed: the fixture must be rejectable when the endpoint IS readable, or the fail-open assertion proves nothing")
	}

	if _, err := v.ValidateCreate(context.Background(), task); err != nil {
		t.Fatalf("an unreadable dependency must not block an otherwise valid write; got %v", err)
	}
}
