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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

func pendingCustom(name, verb string, payload []byte) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type:       fleetv1.ActionTypeCustom,
			CustomType: verb,
			Payload:    &fleetv1.ActionPayload{Raw: payload},
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

// spec.customType rides the assignment (AssignAction.custom_type). It was an
// operator-surface field only, and the verb an adapter acted on was whatever the payload
// named; the two could disagree and nothing on the wire could show it.
func TestWire_AssignActionCarriesTheCustomVerb(t *testing.T) {
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t,
		pendingCustom("t1", "explore-floor", []byte(`{"operation":"explore-floor"}`)),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %+v, want exactly one", cmd.assigns)
	}
	if got := cmd.assigns[0].CustomType; got != "explore-floor" {
		t.Errorf("assign_action custom_type = %q, want spec.customType %q", got, "explore-floor")
	}
	// The payload is the author's and is carried as written: the control plane never
	// writes the verb into it.
	if got := string(cmd.assigns[0].Payload); got != `{"operation":"explore-floor"}` {
		t.Errorf("payload = %q, want it untouched", got)
	}
}

// A Custom action that names no verb sends none: the adapter then reads the payload, as
// before the field existed.
func TestWire_AssignActionWithNoCustomVerbSendsNone(t *testing.T) {
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t,
		pendingCustom("t1", "", []byte(`{"operation":"explore-floor"}`)),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %+v, want exactly one", cmd.assigns)
	}
	if got := cmd.assigns[0].CustomType; got != "" {
		t.Errorf("assign_action custom_type = %q, want empty", got)
	}
}

type customTypeRecordingValidator struct{ actionType, customType string }

func (v *customTypeRecordingValidator) ValidateAction(_ context.Context, _, _, actionType, customType string, _ []byte) (command.ValidateOutcome, error) {
	v.actionType, v.customType = actionType, customType
	return command.ValidateOutcome{Servable: true}, nil
}

// validate_action asks about the same verb the assignment will carry.
func TestValidateAction_ForwardsTheCustomVerb(t *testing.T) {
	act := validateAction()
	act.Spec.Type = fleetv1.ActionTypeCustom
	act.Spec.CustomType = "manual-drive"
	v := &customTypeRecordingValidator{}
	r := &FleetActionReconciler{Validator: v}
	if !r.actionServableBy(context.Background(), act, ptrRobot("amr-1")) {
		t.Fatal("a servable answer must keep the candidate")
	}
	if v.actionType != "Custom" || v.customType != "manual-drive" {
		t.Fatalf("validate_action got (%q, %q), want (Custom, manual-drive)", v.actionType, v.customType)
	}
}
