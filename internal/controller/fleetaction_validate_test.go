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
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// Assignment-time action validation (RFC-0001 §9.2.3). The supported-action catalog is a
// TYPE-level pre-filter — "can this adapter serve actions of this kind at all" — so a stale
// catalog can admit an action the chosen robot cannot actually perform. Asking the adapter
// about the concrete instance turns a failed assignment into a scheduling miss.
//
// The three replies are not interchangeable, and the `unsupported` one is the trap:
// validate_action is an OPTIONAL command, so treating "did not answer the question" as "no"
// would make it effectively mandatory and strand every adapter that has not implemented it.

type stubValidator struct {
	// byRobot maps robot name → outcome. A robot absent from the map is servable.
	byRobot map[string]command.ValidateOutcome
	errFor  map[string]error
	calls   []string
}

func (s *stubValidator) ValidateAction(_ context.Context, _, robotID, _ string, _ []byte) (command.ValidateOutcome, error) {
	s.calls = append(s.calls, robotID)
	if err, ok := s.errFor[robotID]; ok {
		return command.ValidateOutcome{}, err
	}
	if out, ok := s.byRobot[robotID]; ok {
		return out, nil
	}
	return command.ValidateOutcome{Servable: true}, nil
}

func validateAction() *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "pick-1", Namespace: "warehouse-a"},
		Spec: fleetv1.FleetActionSpec{
			Type:    fleetv1.ActionTypeNavigate,
			Payload: &fleetv1.ActionPayload{Raw: []byte(`{"bin":"A-14"}`)},
		},
	}
}

func validateRobot(name string) fleetv1.Robot {
	return fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "warehouse-a"}}
}

func TestValidateAction_ServableRobotIsAccepted(t *testing.T) {
	v := &stubValidator{}
	r := &FleetActionReconciler{Validator: v}
	if !r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1")) {
		t.Fatal("a servable robot must be accepted")
	}
	if len(v.calls) != 1 || v.calls[0] != "amr-1" {
		t.Fatalf("want one validate for amr-1, got %v", v.calls)
	}
}

func TestValidateAction_UnsupportedDoesNotWithholdWork(t *testing.T) {
	// THE TRAP. validate_action is OPTIONAL (§9.2.8). An adapter that has not implemented
	// it replies unsupported, and the control plane must dispatch on the catalog gate alone
	// — exactly as it did before this check existed. Withholding here would make an optional
	// command mandatory and quietly stop work on every adapter that declines it.
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{
		"amr-1": {Unsupported: true, Message: "adapter does not implement validate_action"},
	}}
	r := &FleetActionReconciler{Validator: v}
	if !r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1")) {
		t.Fatal("an adapter that declines an OPTIONAL command must not lose the robot its work")
	}
}

func TestValidateAction_NotServableWithholdsTheRobot(t *testing.T) {
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{
		"amr-1": {Servable: false, Message: "bin A-14 is outside this robot's reach"},
	}}
	r := &FleetActionReconciler{Validator: v}
	if r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1")) {
		t.Fatal("an explicit servable=false must withhold the robot")
	}
}

func TestValidateAction_UnreachableWithholdsTheRobot(t *testing.T) {
	// The case §9.2.3 does not state. validate_action is pure inspection, so dropping the
	// candidate costs nothing and the action stays Pending. Dispatching to an adapter we
	// just failed to reach would commit the assignment and then push assign_action
	// best-effort into the same silence — a bound robot that may never receive its task.
	v := &stubValidator{errFor: map[string]error{"amr-1": errors.New("no ControlStream")}}
	r := &FleetActionReconciler{Validator: v}
	if r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1")) {
		t.Fatal("an unreachable adapter must not have its robot dispatched to")
	}
}

func TestValidateAction_NoValidatorKeepsTheCatalogOnlyBehaviour(t *testing.T) {
	// ControlStream disabled: there is no one to ask. The v0.2 behaviour must be preserved
	// exactly, or turning ControlStream off would stop all dispatch.
	r := &FleetActionReconciler{Validator: nil}
	if !r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1")) {
		t.Fatal("with no validator the catalog gate alone must still dispatch")
	}
}

func TestValidateAction_PayloadIsForwardedNotJustType(t *testing.T) {
	// The whole point of the per-instance check is the PAYLOAD: the catalog already answers
	// the type-level question, so forwarding only the type would make this round trip
	// redundant with the gate it is meant to complement.
	var gotType string
	var gotPayload []byte
	v := &recordingValidator{fn: func(actionType string, payload []byte) {
		gotType, gotPayload = actionType, payload
	}}
	r := &FleetActionReconciler{Validator: v}
	r.actionServableBy(context.Background(), validateAction(), ptrRobot("amr-1"))
	if gotType != string(fleetv1.ActionTypeNavigate) {
		t.Fatalf("action type not forwarded, got %q", gotType)
	}
	if string(gotPayload) != `{"bin":"A-14"}` {
		t.Fatalf("payload not forwarded, got %q", gotPayload)
	}
}

func TestValidateAction_NilPayloadIsNotADereference(t *testing.T) {
	// spec.payload is optional; an action without one must validate rather than panic.
	act := validateAction()
	act.Spec.Payload = nil
	r := &FleetActionReconciler{Validator: &stubValidator{}}
	if !r.actionServableBy(context.Background(), act, ptrRobot("amr-1")) {
		t.Fatal("an action with no payload must still validate")
	}
}

type recordingValidator struct{ fn func(string, []byte) }

func (v *recordingValidator) ValidateAction(_ context.Context, _, _, actionType string, payload []byte) (command.ValidateOutcome, error) {
	v.fn(actionType, payload)
	return command.ValidateOutcome{Servable: true}, nil
}

func ptrRobot(name string) *fleetv1.Robot {
	rob := validateRobot(name)
	return &rob
}

// ── candidate selection ────────────────────────────────────────────────────────────────

// fixedScheduler picks the first robot in the pool, so the test controls the order the
// selection loop sees and can assert that a refused candidate is actually removed.
type fixedScheduler struct{ err error }

func (f *fixedScheduler) SelectRobot(_ *fleetv1.FleetAction, candidates []fleetv1.Robot, _, _, _ bool) (fleetv1.Robot, error) {
	if len(candidates) == 0 {
		return fleetv1.Robot{}, errors.New("no eligible robot")
	}
	if f.err != nil {
		return fleetv1.Robot{}, f.err
	}
	return candidates[0], nil
}

func TestSelectServable_SkipsARefusedCandidateAndTakesTheNext(t *testing.T) {
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{
		"amr-1": {Servable: false, Message: "cannot reach that bin"},
	}}
	r := &FleetActionReconciler{Validator: v, Scheduler: &fixedScheduler{}}

	got, err := r.selectServableRobot(context.Background(), validateAction(),
		[]fleetv1.Robot{validateRobot("amr-1"), validateRobot("amr-2")}, false, false, false)
	if err != nil {
		t.Fatalf("a servable second candidate must be selected: %v", err)
	}
	if got.Name != "amr-2" {
		t.Fatalf("want amr-2 after amr-1 refused, got %q", got.Name)
	}
	if len(v.calls) != 2 {
		t.Fatalf("want one validate per attempt (2), got %v", v.calls)
	}
}

func TestSelectServable_AllRefusedIsASchedulingMissNotAFailure(t *testing.T) {
	// Every candidate refuses. The caller's existing no-robot path must be taken — the
	// action goes back to Pending and waits for a capable robot; it is not failed, and
	// retryCount is not incremented (§9.2.3).
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{
		"amr-1": {Servable: false}, "amr-2": {Servable: false},
	}}
	r := &FleetActionReconciler{Validator: v, Scheduler: &fixedScheduler{}}

	_, err := r.selectServableRobot(context.Background(), validateAction(),
		[]fleetv1.Robot{validateRobot("amr-1"), validateRobot("amr-2")}, false, false, false)
	if err == nil {
		t.Fatal("with every candidate refusing, selection must report no eligible robot")
	}
}

func TestSelectServable_TerminatesOnAnEmptyPool(t *testing.T) {
	// A loop over candidates that removes one per pass must not spin when the pool starts
	// empty — the scheduler's own error is the answer.
	r := &FleetActionReconciler{Validator: &stubValidator{}, Scheduler: &fixedScheduler{}}
	if _, err := r.selectServableRobot(context.Background(), validateAction(), nil, false, false, false); err == nil {
		t.Fatal("an empty candidate pool must report no eligible robot")
	}
}
