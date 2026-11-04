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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The generated Robot CRD validates status.stops on a real API server (a fake client ignores CRD
// validation): at most 256 entries, a carrier on every stop but a Reported or EndState one, an
// event only on an EndState one, and a reported ID up to 320 characters.
func TestEnvtest_RobotStops_CRDValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	robot := envtestValidRobot(ns, "r1", "z")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatal(err)
	}
	carrier := &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "z"}
	stop := func(id string, scope fleetv1.StopScope, c *fleetv1.StopCarrier) fleetv1.RobotStop {
		return fleetv1.RobotStop{ID: id, Scope: scope, Carrier: c, SentAt: metav1.Now()}
	}
	many := func(n int) []fleetv1.RobotStop {
		out := make([]fleetv1.RobotStop, n)
		for i := range out {
			out[i] = stop(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), fleetv1.StopScopeZone, carrier)
		}
		return out
	}
	withEvent := stop("e1", fleetv1.StopScopeRobot, &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "r1"})
	withEvent.Event = fleetv1.StopEventRegister
	endState := stop("e2", fleetv1.StopScopeEndState, nil)
	endState.Event = fleetv1.StopEventLocalStop
	for _, tc := range []struct {
		name   string
		stops  []fleetv1.RobotStop
		refuse string
	}{
		{"256-entries-admitted", many(256), ""},
		{"257-entries-refused", many(257), "Too many"},
		{"zone-stop-without-carrier-refused", []fleetv1.RobotStop{stop("z1", fleetv1.StopScopeZone, nil)}, "carrier"},
		{"reported-stop-with-carrier-refused", []fleetv1.RobotStop{stop("p1", fleetv1.StopScopeReported, carrier)}, "carrier"},
		{"event-on-a-robot-stop-refused", []fleetv1.RobotStop{withEvent}, "event"},
		{"end-state-stop-with-event-admitted", []fleetv1.RobotStop{endState}, ""},
		{"robot-carrier-kind-admitted", []fleetv1.RobotStop{stop("r", fleetv1.StopScopeRobot,
			&fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "r1"})}, ""},
		{"300-character-reported-id-admitted", []fleetv1.RobotStop{stop("estop-"+strings.Repeat("a", 294), fleetv1.StopScopeReported, nil)}, ""},
		{"321-character-id-refused", []fleetv1.RobotStop{stop(strings.Repeat("a", 321), fleetv1.StopScopeReported, nil)}, "may not be more than 320"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fleetv1.Robot{}
			if err := envK8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "r1"}, r); err != nil {
				t.Fatal(err)
			}
			r.Status.Stops = tc.stops
			err := envK8s.Status().Update(ctx, r)
			switch {
			case tc.refuse == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refuse != "" && err == nil:
				t.Fatalf("admitted; want a refusal naming %q", tc.refuse)
			case tc.refuse != "" && !strings.Contains(err.Error(), tc.refuse):
				t.Fatalf("refused, but not for %q: %v", tc.refuse, err)
			}
		})
	}
}
