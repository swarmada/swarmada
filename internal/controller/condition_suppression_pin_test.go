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
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A controller-owned condition may hold an action back from rows A15, A17 and A23 only (RFC-0001
// safety.md, the preamble of the transition table). The one such condition the reference writes,
// ExternalDependenciesReady=False, holds dispatch (A15); it must not hold back a row that guards a
// stop, and a running report still records the start.

func withDependenciesNotReady(a *fleetv1.FleetAction) *fleetv1.FleetAction {
	apimeta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
		Type: conditionExternalDependenciesReady, Status: metav1.ConditionFalse,
		Reason: "SensorStreamUnavailable", Message: "a stream the action needs is not declared",
	})
	return a
}

func TestConditionSuppression_AStopStillPausesTheWork(t *testing.T) {
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress} {
		t.Run(string(phase), func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
			r, c := newActionReconciler(t,
				withDependenciesNotReady(assignedAction("t1", "r1", phase, 4, lease)),
				robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"))
			reconcileAction(t, r, "t1")
			// The phase alone: what A1 keeps is pinned by the A1 rows.
			if got := getAction(t, c, "t1"); got.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("phase=%s under a stop with the condition False; want Paused (A1 is not suppressible)", got.Status.Phase)
			}
		})
	}
}

func TestConditionSuppression_ARunningReportRecordsTheStart(t *testing.T) {
	i, c := newActionStatusIngestor(t, withDependenciesNotReady(assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil)))
	ingest(t, i, &fav1.ActionStatusUpdate{ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5)})
	if got := getAction(t, c, "t1"); got.Status.StartedAt == nil {
		t.Fatalf("a running report on an action carrying the condition recorded no start (phase %s)", got.Status.Phase)
	}
}
