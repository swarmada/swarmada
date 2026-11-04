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

// AN ACTION REPORT IS AUTHORIZED BY THE ASSIGNED ROBOT'S WIRE ROBOT_ID.
//
// The assignment names the Robot object; the stream authorizer judges the robot_id the
// adapter announced. For a robot admitted under another name (`swarmctl admit --name`)
// they differ, so asking the authorizer about the object name asks about a robot_id no
// adapter announced: the robot's own adapter is refused, and a stream serving whichever
// robot does carry that id as its robot_id would be accepted.

// streamServing is a stream that may act on exactly the robot_ids given.
func streamServing(ids ...string) func(context.Context, string) bool {
	return func(_ context.Context, id string) bool {
		for _, want := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
}

func TestActionStatusWireID_RenamedRobotsOwnAdapterAdvancesTheAction(t *testing.T) {
	i, c := newActionStatusIngestor(t,
		robotAt("amr-1", "wire-7", ""),
		assignedAction("t1", "amr-1", fleetv1.ActionPhaseAssigned, 5, nil))
	if err := i.IngestActionStatus(context.Background(), actionNS, streamServing("wire-7"), &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := getAction(t, c, "t1").Status.Phase; got != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress: the robot's own adapter (robot_id wire-7) was refused", got)
	}
}

func TestActionStatusWireID_StreamServingTheObjectNameAsAnIDIsRefused(t *testing.T) {
	i, c := newActionStatusIngestor(t,
		robotAt("amr-1", "wire-7", ""),
		assignedAction("t1", "amr-1", fleetv1.ActionPhaseAssigned, 5, nil))
	if err := i.IngestActionStatus(context.Background(), actionNS, streamServing("amr-1"), &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: u64(5),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := getAction(t, c, "t1").Status.Phase; got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned unchanged: a stream serving robot_id amr-1 ended the action "+
			"of the Robot named amr-1, whose robot_id is wire-7", got)
	}
}
