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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A live terminal report from another run of the action (its fencing token names an older
// assignment) is refused, and the refusal is raised on the bound robot the stream serves, never
// on the action, which is often a later run created under the same name. A stream that does not
// serve the bound robot raises nothing.
func TestIngest_FencedOutReportIsRefusedOnTheBoundRobot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authorize func(context.Context, string) bool
		onRobot   int
	}{
		{"authorized-stream", func(context.Context, string) bool { return true }, 1},
		{"unauthorized-stream", func(context.Context, string) bool { return false }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))
			ev := &eventTargets{}
			i.Recorder = ev
			if err := i.IngestActionStatus(context.Background(), actionNS, tc.authorize, &fav1.ActionStatusUpdate{
				ActionId: "t1", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: u64(4),
			}); err != nil {
				t.Fatal(err)
			}
			if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseInProgress {
				t.Fatalf("a fenced-out report moved the action to %s", ft.Status.Phase)
			}
			onRobot, onAction := 0, 0
			for _, e := range ev.got {
				if e.reason != EventActionReportRefused {
					continue
				}
				switch e.kind {
				case "*v1.Robot":
					if e.name == "r1" {
						onRobot++
					}
				case "*v1.FleetAction":
					onAction++
				}
			}
			if onRobot != tc.onRobot || onAction != 0 {
				t.Fatalf("ActionReportRefused on robot r1 %d time(s) and on the action %d time(s); want %d and 0: %+v",
					onRobot, onAction, tc.onRobot, ev.got)
			}
		})
	}
}

// The manager's ingestor is built with an API reader: retained outcomes are decided only on the
// API server's copy of the action.
func TestNewActionStatusIngestor_RequiresAnAPIReader(t *testing.T) {
	i, c := newActionStatusIngestor(t)
	if _, err := NewActionStatusIngestor(c, nil, nil, nil); err == nil {
		t.Fatal("an ingestor with no API reader was built")
	}
	got, err := NewActionStatusIngestor(c, i.Client, nil, nil)
	if err != nil || got.APIReader == nil {
		t.Fatalf("an ingestor with an API reader: %v, APIReader set %v", err, got != nil && got.APIReader != nil)
	}
}
