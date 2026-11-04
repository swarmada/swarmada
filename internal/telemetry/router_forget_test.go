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

package telemetry

import (
	"context"
	"testing"
	"time"
)

// ForgetRobot is called when a Robot is recreated under the same robot_id. The
// last-frame clock belongs to the old object as much as the projector state does:
// kept, it reports the new Robot as having been silent since the old one's last frame,
// and the Robot reconciler marks it Offline before its first frame can arrive.
func TestRouter_ForgetRobotDropsTheLastFrameClock(t *testing.T) {
	start := time.Now()
	now := start
	r := NewRouter(func(string) PlaneConfig { return PlaneConfig{} }, nil, WithClock(func() time.Time { return now }))
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-b"}); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Hour)

	r.ForgetRobot("ns-a", "rid-1")
	if _, seen := r.Silence("ns-a", "rid-1"); seen {
		t.Fatal("a forgotten robot still reports a last frame from the previous object")
	}
	if _, ok := r.LastFrame("ns-a", "rid-1"); ok {
		t.Fatal("LastFrame still reports the previous object's frame")
	}
	if _, seen := r.Silence("ns-b", "rid-1"); !seen {
		t.Fatal("forgetting the robot in ns-a dropped the same robot_id's clock in ns-b")
	}
}
