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

	"k8s.io/apimachinery/pkg/types"

	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// A Robot deleted and recreated under the same wire robot_id is a new object with an
// empty status. The position observer's memo of the zone it last wrote belongs to the
// OLD object: kept, it makes the first frame in the same zone look like no transition,
// nothing writes currentZone, and candidate filter 2 withholds the new robot for as
// long as this process runs. The resetter that clears the projector on a change of
// object identity must clear this memo too.
func TestZoneObserver_RecreatedRobotGetsItsZoneWritten(t *testing.T) {
	first := robotAt("r1", "rid1", "")
	first.UID = types.UID("original-0000-0000-0000-000000000001")
	z, c := newZoneController(t, first)
	z.setZones(actionNS, []zone.Candidate{{Name: "z1", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})
	resetter := &RobotProjectionResetter{}
	if f, ok := any(z).(ProjectionForgetter); ok {
		resetter.SetForgetter(f)
	}
	ctx := context.Background()
	frame := telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}}

	old := getRobot(t, c, "r1", actionNS)
	if old.UID == "" {
		t.Fatal("fixture has no UID; the resetter keys on it")
	}
	resetter.Observe(actionNS, "rid1", old.UID)
	z.ObservePosition(ctx, frame)
	if got := getRobot(t, c, "r1", actionNS); got.Status.CurrentZone != "z1" {
		t.Fatalf("currentZone = %q, want z1", got.Status.CurrentZone)
	}

	// Delete and recreate: same name, same robot_id, a new object with no status.
	if err := c.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	fresh := robotAt("r1", "rid1", "")
	fresh.UID = types.UID("recreated-0000-0000-0000-000000000002")
	if err := c.Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	created := getRobot(t, c, "r1", actionNS)
	if created.UID == "" || created.UID == old.UID {
		t.Fatalf("recreated robot UID = %q (old %q); the recreation is not a new object", created.UID, old.UID)
	}
	resetter.Observe(actionNS, "rid1", created.UID) // the Robot reconciler's call on the new object

	z.ObservePosition(ctx, frame)
	if got := getRobot(t, c, "r1", actionNS); got.Status.CurrentZone != "z1" {
		t.Fatalf("recreated robot: currentZone = %q, want z1 — the previous object's zone memo "+
			"suppressed the establishing write", got.Status.CurrentZone)
	}
}
