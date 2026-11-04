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

// FirstFrame is the baseline of a leader that finds a robot in Resuming and did not accept
// its clear (internal/safety.Dispatcher.ConfirmResume): the first frame this process received
// from the robot. Later frames move LastFrame, never FirstFrame.
func TestRouter_FirstFrameStaysTheFirst(t *testing.T) {
	start := time.Now()
	now := start
	r := NewRouter(func(string) PlaneConfig { return PlaneConfig{} }, nil, WithClock(func() time.Time { return now }))

	if at, ok := r.FirstFrame("ns-a", "rid-1"); ok {
		t.Fatalf("unheard robot: FirstFrame = %s, true; want no frame", at)
	}
	now = start.Add(time.Minute)
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	first := now
	now = start.Add(2 * time.Minute)
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	if at, ok := r.FirstFrame("ns-a", "rid-1"); !ok || !at.Equal(first) {
		t.Fatalf("FirstFrame = %s, %v; want %s, true", at, ok, first)
	}
	if at, _ := r.LastFrame("ns-a", "rid-1"); !at.Equal(now) {
		t.Fatalf("LastFrame = %s; want %s", at, now)
	}
	if _, ok := r.FirstFrame("ns-b", "rid-1"); ok {
		t.Fatal("a frame in ns-a stamped the same robot_id's first frame in ns-b")
	}
}

// A Robot recreated under the same robot_id is a new robot: its first frame is its own, not
// the old object's.
func TestRouter_ForgetRobotDropsTheFirstFrame(t *testing.T) {
	start := time.Now()
	now := start
	r := NewRouter(func(string) PlaneConfig { return PlaneConfig{} }, nil, WithClock(func() time.Time { return now }))
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}

	r.ForgetRobot("ns-a", "rid-1")
	if _, ok := r.FirstFrame("ns-a", "rid-1"); ok {
		t.Fatal("FirstFrame still reports the previous object's frame")
	}
	now = start.Add(time.Hour)
	if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
		t.Fatal(err)
	}
	if at, ok := r.FirstFrame("ns-a", "rid-1"); !ok || !at.Equal(now) {
		t.Fatalf("FirstFrame after ForgetRobot = %s, %v; want the new object's %s", at, ok, now)
	}
}
