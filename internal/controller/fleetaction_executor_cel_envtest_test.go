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

// Admission coverage for the ONE CEL rule that carries Option B.
//
// The external-action decision rests on a single claim: the eight robot-scheduling
// fields plus payload are not "conditionally meaningful" but "conditionally
// FORBIDDEN", so a field a caller cannot set is self-documenting at apply time
// rather than silently doing nothing. That claim is true only if the rule on
// FleetActionSpec is actually in effect at admission.
//
// Until this file, nothing proved it was. The rule was exercised only by a
// separate Python demo lint, which RE-IMPLEMENTS R1 and R2 (below) in Python
// against a hand-maintained schema — a second opinion, not the API server's.
// A marker that failed to regenerate, a CEL path typo, or a rule the API server
// rejects as uncompilable all read identically to a correct one under any client
// that does not talk to an API server, and the fake-client suite accepts the object
// either way.
//
// Both halves are asserted throughout, because a rule that rejects everything
// passes a rejection-only test while making External unusable, and a rule scoped
// one field too wide would reject the ordinary robot actions that are the other
// 99% of the fleet.

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// executorRuleMessage is a distinctive substring of the rule's own message. It is
// matched rather than the whole message so a wording change does not break the
// test, but it names both halves so a DIFFERENT rule firing cannot satisfy it.
const executorRuleMessage = "spec.executor and spec.type=External require each other"

// externalActionSpec is the minimal admissible External action: the discriminator
// and nothing else. The "FleetAction — external, standalone" example in
// Swarmada_Draft4_External_Action_Spec_Examples.md, reduced to what the rule sees.
func externalActionSpec() fleetv1.FleetActionSpec {
	return fleetv1.FleetActionSpec{
		Type:     fleetv1.ActionTypeExternal,
		Executor: &fleetv1.ActionExecutor{EndpointRef: "plate-reader"},
	}
}

// robotActionSpec is the contrast case from "FleetAction — robot, for
// contrast" in the same document: the same resource with no executor.
func robotActionSpec() fleetv1.FleetActionSpec {
	return fleetv1.FleetActionSpec{Type: fleetv1.ActionTypePatrol}
}

func newAction(ns string, spec fleetv1.FleetActionSpec) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "fa-", Namespace: ns},
		Spec:       spec,
	}
}

// ── R1: the executor block and type: External imply each other ────────────────

func TestEnvtest_ExecutorAndExternalTypeImplyEachOther(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// Accepted: both together.
	if err := envK8s.Create(ctx, newAction(ns, externalActionSpec())); err != nil {
		t.Fatalf("the rule rejected the canonical External action, which it must permit: %v", err)
	}

	// Accepted: neither — the ordinary robot action, which must not be disturbed.
	if err := envK8s.Create(ctx, newAction(ns, robotActionSpec())); err != nil {
		t.Fatalf("the rule rejected an ordinary robot action with no executor: %v", err)
	}

	// Rejected: an executor under a robot type. This is the case that would
	// otherwise dispatch to an endpoint while every operator surface prints Patrol.
	orphanExecutor := externalActionSpec()
	orphanExecutor.Type = fleetv1.ActionTypePatrol
	mustRejectCleanly(t, envK8s.Create(ctx, newAction(ns, orphanExecutor)), executorRuleMessage)

	// Rejected: type: External with no executor. Nothing would tell this action
	// where to run, and the scheduler's early return would strand it Pending
	// forever rather than failing it at apply time.
	orphanType := fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeExternal}
	mustRejectCleanly(t, envK8s.Create(ctx, newAction(ns, orphanType)), executorRuleMessage)
}

// ── R2: the eight scheduling fields plus payload are forbidden with an executor ──

// The eight fields go inert TOGETHER, which is what let one rule replace eight —
// so the test asserts them one at a time. A rule that named seven of the nine
// would pass any test that set them as a group.
func TestEnvtest_ExecutorForbidsTheRobotSchedulingFields(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	accept := true
	pct := int32(30)
	secs := int32(120)

	cases := []struct {
		field string
		set   func(*fleetv1.FleetActionSpec)
	}{
		{"requiredCapabilities", func(s *fleetv1.FleetActionSpec) {
			s.RequiredCapabilities = []string{"navigation"}
		}},
		{"constraints", func(s *fleetv1.FleetActionSpec) {
			s.Constraints = map[string]float64{"maxPayloadKg": 30}
		}},
		{"acceptDegradedCapabilities", func(s *fleetv1.FleetActionSpec) {
			s.AcceptDegradedCapabilities = &accept
		}},
		{"estimatedDurationSeconds", func(s *fleetv1.FleetActionSpec) {
			s.EstimatedDurationSeconds = &secs
		}},
		{"robotSelector", func(s *fleetv1.FleetActionSpec) { s.RobotSelector = "amr-1" }},
		{"minBatteryPct", func(s *fleetv1.FleetActionSpec) { s.MinBatteryPct = &pct }},
		{"preferredRobot", func(s *fleetv1.FleetActionSpec) { s.PreferredRobot = "amr-1" }},
		{"preferredManufacturer", func(s *fleetv1.FleetActionSpec) { s.PreferredManufacturer = "acme" }},
		// payload is the ninth and the one that, under "Option B's costs — three,
		// and one is real", Swarmada_Draft4_ComputeAction_vs_FleetAction_Decision.md
		// calls a genuine wart: it is a SECOND parameter mechanism beside
		// executor.parameters, so leaving it settable would give one resource two
		// ways to pass arguments with no rule about which wins.
		{"payload", func(s *fleetv1.FleetActionSpec) {
			s.Payload = &fleetv1.ActionPayload{}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.field+"/rejected with an executor", func(t *testing.T) {
			spec := externalActionSpec()
			tc.set(&spec)
			mustRejectCleanly(t, envK8s.Create(ctx, newAction(ns, spec)), executorRuleMessage)
		})

		// THE OTHER HALF. Each of these nine is a field ordinary robot actions
		// use every day. A rule written without the `!has(self.executor) ||`
		// guard would reject the whole fleet, and a rejection-only test would
		// call that a pass.
		t.Run(tc.field+"/accepted without an executor", func(t *testing.T) {
			spec := robotActionSpec()
			tc.set(&spec)
			if err := envK8s.Create(ctx, newAction(ns, spec)); err != nil {
				t.Fatalf("spec.%s was rejected on a ROBOT action, where it is the normal shape: %v",
					tc.field, err)
			}
		})
	}
}

// spec.zone is the one scheduling-adjacent field an External action MAY keep: it
// names the edge zone context for endpoint resolution rather than a robot
// constraint (api/v1/fleetaction_types.go, FleetActionSpec.Executor). It is
// deliberately absent from the nine, so a rule that swept it in would be wrong in
// the direction no rejection test would catch.
func TestEnvtest_ExecutorPermitsZone(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	spec := externalActionSpec()
	spec.Zone = "yard-west"
	if err := envK8s.Create(ctx, newAction(ns, spec)); err != nil {
		t.Fatalf("spec.zone was rejected on an External action; it names the edge zone "+
			"context for endpoint resolution and is not one of the nine forbidden fields: %v", err)
	}
}
