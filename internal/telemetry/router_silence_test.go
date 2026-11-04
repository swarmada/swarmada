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

// The per-robot last-frame clock: every frame stamps it, including one the projector
// finds unchanged; a robot not yet heard from is timed from the router's start; and
// robots are kept apart by namespace as well as robot_id.
func TestRouter_SilenceIsPerRobotAndCountsEveryFrame(t *testing.T) {
	start := time.Now()
	now := start
	r := NewRouter(func(string) PlaneConfig { return PlaneConfig{} }, nil, WithClock(func() time.Time { return now }))

	now = start.Add(10 * time.Second)
	if d, seen := r.Silence("ns-a", "rid-1"); seen || d != 10*time.Second {
		t.Fatalf("unheard robot: silence=%s seen=%v, want 10s from the router's start and unseen", d, seen)
	}

	for i := 0; i < 3; i++ { // identical frames: the projector writes at most once, the clock every time
		now = now.Add(time.Second)
		if err := r.Ingest(context.Background(), Frame{RobotID: "rid-1", Namespace: "ns-a"}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	last := now
	now = last.Add(7 * time.Second)
	if d, seen := r.Silence("ns-a", "rid-1"); !seen || d != 7*time.Second {
		t.Fatalf("silence=%s seen=%v, want 7s since the last frame", d, seen)
	}
	if _, seen := r.Silence("ns-b", "rid-1"); seen {
		t.Fatal("a frame in ns-a stamped the same robot_id in ns-b")
	}
}
