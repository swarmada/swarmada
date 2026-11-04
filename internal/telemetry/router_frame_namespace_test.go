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

package telemetry_test

import (
	"context"
	"sync"
	"testing"

	"github.com/swarmada/swarmada/internal/telemetry"
)

// A robot with a bare wire robot_id is governed by the telemetry configuration of
// the namespace its stream is served in: the TSDB sink AND the status write-policy.
// The "" plane — whose configuration the manager resolves with an unscoped List —
// is not consulted for a frame that carries a namespace.
func TestRouter_BareRobotIDIsGovernedByItsStreamNamespace(t *testing.T) {
	sinkA, sinkB := &recordingTSDB{}, &recordingTSDB{}
	factory := func(sinkType, _ string) telemetry.TSDBWriter {
		switch sinkType {
		case "A":
			return sinkA
		case "B":
			return sinkB
		default:
			return telemetry.NoopWriter{}
		}
	}
	var mu sync.Mutex
	resolved := map[string]int{}
	resolver := func(ns string) telemetry.PlaneConfig {
		mu.Lock()
		resolved[ns]++
		mu.Unlock()
		switch ns {
		case "ns-a":
			return telemetry.PlaneConfig{SinkType: "A",
				Projector: telemetry.Config{BatteryThresholds: []int32{15, 30}}}
		case "ns-b":
			// A threshold at 45: a drop from 50 to 40 is material here and not in ns-a.
			return telemetry.PlaneConfig{SinkType: "B",
				Projector: telemetry.Config{BatteryThresholds: []int32{15, 30, 45}}}
		default:
			return telemetry.PlaneConfig{}
		}
	}
	status := &recordingStatusSink{}
	r := telemetry.NewRouter(resolver, status, telemetry.WithSinkFactory(factory))

	ctx := context.Background()
	for _, pct := range []int32{50, 40} {
		for _, ns := range []string{"ns-a", "ns-b"} {
			if err := r.Ingest(ctx, telemetry.Frame{RobotID: "chassis-7", Namespace: ns, BatteryPct: i32(pct)}); err != nil {
				t.Fatalf("ingest %s at %d%%: %v", ns, pct, err)
			}
		}
	}

	if sinkA.count() != 2 || sinkB.count() != 2 {
		t.Fatalf("TSDB routing: sinkA=%d sinkB=%d, want 2 and 2 (each robot's own namespace's sink)",
			sinkA.count(), sinkB.count())
	}
	writes := map[string]int{}
	status.mu.Lock()
	for _, u := range status.upds {
		if u.BatteryPct != nil {
			writes[u.Namespace]++
		}
	}
	status.mu.Unlock()
	if writes["ns-a"] != 1 || writes["ns-b"] != 2 {
		t.Fatalf("battery status writes = %v, want ns-a:1 (50->40 inside its bucket) and ns-b:2 "+
			"(50->40 crosses its 45 threshold): each robot must get its own namespace's write-policy", writes)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := resolved[""]; n != 0 {
		t.Fatalf("the \"\" plane was resolved %d times for frames that carried a namespace", n)
	}
}

// A frame with no namespace still routes by the namespace in its RobotID, and a
// bare one to the "" plane: that fallback is unchanged.
func TestRouter_FrameWithNoNamespaceFallsBackToItsRobotID(t *testing.T) {
	var mu sync.Mutex
	var resolved []string
	r := telemetry.NewRouter(func(ns string) telemetry.PlaneConfig {
		mu.Lock()
		resolved = append(resolved, ns)
		mu.Unlock()
		return telemetry.PlaneConfig{}
	}, &recordingStatusSink{})
	ctx := context.Background()
	for _, id := range []string{"ns-c/r1", "bare-r2"} {
		if err := r.Ingest(ctx, telemetry.Frame{RobotID: id, BatteryPct: i32(80)}); err != nil {
			t.Fatalf("ingest %s: %v", id, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resolved) != 2 || resolved[0] != "ns-c" || resolved[1] != "" {
		t.Fatalf("resolved planes = %q, want [ns-c \"\"]", resolved)
	}
}
