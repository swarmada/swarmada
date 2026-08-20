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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The failure mode the per-field charging merge introduced (Round-4 RobotClass change).
//
// The merge used to inherit spec.charging as a WHOLE BLOCK: a Robot that set any charging
// field kept its own block entire and the class's was discarded silently. Per-field
// inheritance is the better behaviour, but it creates a pair that whole-block inheritance
// could never produce — a Robot-set minBatteryPctToCharge combined with a class-supplied
// targetBatteryPct — and that pair can violate the RobotChargingConfig CEL rule
// (targetBatteryPct must exceed minBatteryPctToCharge).
//
// This is an inconsistent declaration failing CLOSED, which is the right outcome. What has
// to be true for it to be actionable is that the operator can tell WHICH field to change,
// and that is not decided by any code in this package: the defaulter mutates, and the API
// server's CEL rule judges the result. So the object mergeRobotClass actually emits is
// posted to a real API server here.

func chargingClass(min, target *int32) *fleetv1.RobotClass {
	return &fleetv1.RobotClass{
		Spec: fleetv1.RobotClassSpec{
			DefaultChargingConfig: &fleetv1.ClassChargingConfig{
				MinBatteryPctToCharge: min,
				TargetBatteryPct:      target,
			},
		},
	}
}

// The unit half: the merge really does produce the inconsistent pair. Without this, the
// envtest below could pass for the wrong reason (a hand-built fixture nothing produces).
func TestMergeRobotClass_PerFieldChargingCanProduceAnInconsistentPair(t *testing.T) {
	robot := &fleetv1.Robot{
		Spec: fleetv1.RobotSpec{
			Charging: &fleetv1.RobotChargingConfig{MinBatteryPctToCharge: i32(90)},
		},
	}
	// Class target (80) is BELOW the robot's own floor (90).
	mergeRobotClass(robot, chargingClass(i32(15), i32(80)))

	ch := robot.Spec.Charging
	if ch.MinBatteryPctToCharge == nil || *ch.MinBatteryPctToCharge != 90 {
		t.Fatalf("robot's own minBatteryPctToCharge must win, got %v", ch.MinBatteryPctToCharge)
	}
	if ch.TargetBatteryPct == nil || *ch.TargetBatteryPct != 80 {
		t.Fatalf("class targetBatteryPct must be inherited per-field, got %v", ch.TargetBatteryPct)
	}
	if *ch.TargetBatteryPct > *ch.MinBatteryPctToCharge {
		t.Fatal("fixture no longer produces the inconsistent pair this test exists to cover")
	}
}

// The admission half: the API server rejects that exact object, and says which fields.
func TestRobotChargingCEL_InheritedPairIsRejectedAndNamesTheFields(t *testing.T) {
	requireWebhookEnvtest(t)
	ns := webhookEnvtestNamespace(t)

	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "amr-", Namespace: ns},
		Spec: fleetv1.RobotSpec{
			Zone:         "zone-a",
			Manufacturer: "acme",
			Model:        "hauler",
			Adapter:      fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0.0"},
			Charging:     &fleetv1.RobotChargingConfig{MinBatteryPctToCharge: i32(90)},
		},
	}
	mergeRobotClass(robot, chargingClass(i32(15), i32(80)))

	err := whEnvK8s.Create(context.Background(), robot)
	if err == nil {
		t.Fatal("an inconsistent charging pair was ACCEPTED; the CEL rule is not in effect — " +
			"a robot would dock at 90% and leave at 80%, never reaching its target")
	}
	msg := err.Error()
	// An operator reading this has to know which of the two numbers to change. A bare
	// "spec.charging is invalid" would fail closed correctly and still be unactionable —
	// especially here, where one of the two values came from the CLASS, not from the Robot
	// the operator is editing.
	for _, want := range []string{"targetBatteryPct", "minBatteryPctToCharge"} {
		if !strings.Contains(msg, want) {
			t.Errorf("admission error does not name %q, so an operator cannot act on it.\n  got: %v",
				want, err)
		}
	}
	if !strings.Contains(msg, "spec.charging") {
		t.Errorf("admission error does not name the field path spec.charging.\n  got: %v", err)
	}
}

// The acceptance case. A rule that rejects every charging block would pass the test above
// while making the field unusable.
func TestRobotChargingCEL_ConsistentInheritedPairIsAccepted(t *testing.T) {
	requireWebhookEnvtest(t)
	ns := webhookEnvtestNamespace(t)

	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "amr-", Namespace: ns},
		Spec: fleetv1.RobotSpec{
			Zone:         "zone-a",
			Manufacturer: "acme",
			Model:        "hauler",
			Adapter:      fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0.0"},
			Charging:     &fleetv1.RobotChargingConfig{MinBatteryPctToCharge: i32(15)},
		},
	}
	// Class target (90) is above the robot's floor (15): the ordinary case.
	mergeRobotClass(robot, chargingClass(i32(20), i32(90)))

	if err := whEnvK8s.Create(context.Background(), robot); err != nil {
		t.Fatalf("a consistent inherited charging pair was rejected: %v", err)
	}
	if got := robot.Spec.Charging; *got.MinBatteryPctToCharge != 15 || *got.TargetBatteryPct != 90 {
		t.Errorf("stored pair = (min %d, target %d), want (15, 90)",
			*got.MinBatteryPctToCharge, *got.TargetBatteryPct)
	}
}
