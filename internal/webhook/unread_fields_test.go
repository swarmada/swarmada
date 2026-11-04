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

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A served field no controller acts on is admitted with a warning that says so. The
// fields are RFC-0001's maturity-table list; see unread_fields.go.

const noEffect = "setting this field has no effect in this release"

// warnedAbout returns the warnings that name path and carry the no-effect sentence.
func warnedAbout(ws admission.Warnings, path string) []string {
	var out []string
	for _, w := range ws {
		if strings.HasPrefix(w, path+":") && strings.Contains(w, noEffect) {
			out = append(out, w)
		}
	}
	return out
}

func TestUnreadFields_OnUnresolvedParameterWarnsOnFleetAction(t *testing.T) {
	fail := fleetv1.UnresolvedParameterFail
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "warehouse-a"},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeExternal,
			Executor: &fleetv1.ActionExecutor{EndpointRef: "plate-reader", OnUnresolvedParameter: &fail}},
	}
	ws, err := (&FleetActionValidator{}).ValidateCreate(context.Background(), action)
	if err != nil {
		t.Fatalf("the field is admitted, not refused: %v", err)
	}
	if len(warnedAbout(ws, "spec.executor.onUnresolvedParameter")) != 1 {
		t.Errorf("warnings = %q, want one for spec.executor.onUnresolvedParameter", ws)
	}

	action.Spec.Executor.OnUnresolvedParameter = nil
	if ws, _ := (&FleetActionValidator{}).ValidateCreate(context.Background(), action); len(ws) != 0 {
		t.Errorf("unset field: warnings = %q, want none", ws)
	}
}

func TestUnreadFields_OnUnresolvedParameterWarnsOnFleetTaskMember(t *testing.T) {
	omit := fleetv1.UnresolvedParameterOmit
	member := externalMember("read", "plate-reader")
	member.Action.Executor.OnUnresolvedParameter = &omit
	task := ftWith("t1", member)

	ws, err := validatorWith(t, endpoint("plate-reader", fleetv1.ExecutorContractApp)).
		ValidateCreate(context.Background(), task)
	if err != nil {
		t.Fatalf("the field is admitted, not refused: %v", err)
	}
	if len(warnedAbout(ws, "spec.actions[0].action.executor.onUnresolvedParameter")) != 1 {
		t.Errorf("warnings = %q, want one for the member's executor", ws)
	}
}

func TestUnreadFields_RobotFieldsWarnAtCreate(t *testing.T) {
	speed, payload, pct := 0.5, 20.0, int32(30)
	robot := gateRobot()
	robot.Spec.Constraints = &fleetv1.ClassConstraints{MaxSpeedMs: &speed, MaxPayloadKg: &payload}
	robot.Spec.Charging = &fleetv1.RobotChargingConfig{MinBatteryPctToCharge: &pct}

	ws, err := gateFor(t, gateAdapter()).ValidateCreate(context.Background(), robot)
	if err != nil {
		t.Fatalf("the robot is admitted, not refused: %v", err)
	}
	for _, path := range []string{"spec.constraints.maxSpeedMs", "spec.constraints.maxPayloadKg"} {
		got := warnedAbout(ws, path)
		if len(got) != 1 || !strings.Contains(got[0], "the adapter must enforce it") {
			t.Errorf("%s: warnings = %q, want one saying the adapter must enforce it", path, ws)
		}
	}
	if len(warnedAbout(ws, "spec.charging.minBatteryPctToCharge")) != 1 {
		t.Errorf("warnings = %q, want one for spec.charging.minBatteryPctToCharge", ws)
	}
	if len(warnedAbout(ws, "spec.charging.targetBatteryPct")) != 0 {
		t.Errorf("an unset field was warned about: %q", ws)
	}
}

// On update only a changed field is warned about: the defaulter and the control
// plane write Robots, and they are not the author the warning is for.
func TestUnreadFields_RobotWarnsOnUpdateOnlyWhenTheFieldChanges(t *testing.T) {
	speed := 0.5
	old := gateRobot()
	old.Spec.Constraints = &fleetv1.ClassConstraints{MaxSpeedMs: &speed}
	g := gateFor(t, gateAdapter())

	relabelled := old.DeepCopy()
	relabelled.Labels = map[string]string{"aisle": "7"}
	if ws, err := g.ValidateUpdate(context.Background(), old, relabelled); err != nil || len(ws) != 0 {
		t.Errorf("label update: warnings = %q, err = %v; want none", ws, err)
	}

	faster := old.DeepCopy()
	v := 1.5
	faster.Spec.Constraints.MaxSpeedMs = &v
	ws, err := g.ValidateUpdate(context.Background(), old, faster)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnedAbout(ws, "spec.constraints.maxSpeedMs")) != 1 {
		t.Errorf("changed maxSpeedMs: warnings = %q, want one", ws)
	}
}

func TestUnreadFields_DirectRegisterWarns(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Provisioning.Mode = fleetv1.ProvisioningModeDirectRegister
	ws, err := validateSwarmadaConfig(cfg)
	if err != nil {
		t.Fatalf("DirectRegister is admitted, not refused: %v", err)
	}
	if len(warnedAbout(ws, "spec.provisioning.mode")) != 1 {
		t.Errorf("warnings = %q, want one for spec.provisioning.mode", ws)
	}

	cfg.Spec.Provisioning.Mode = fleetv1.ProvisioningModeTwoPhase
	if ws, _ := validateSwarmadaConfig(cfg); len(warnedAbout(ws, "spec.provisioning.mode")) != 0 {
		t.Errorf("TwoPhase: warnings = %q, want none for the mode", ws)
	}
}
