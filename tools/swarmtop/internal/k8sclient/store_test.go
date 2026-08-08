// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import "testing"

// notify() must coalesce: many changes with no reader collapse into a single
// pending nudge, and must never block the producer goroutine.
func TestNotify_Coalesces(t *testing.T) {
	s := NewStoreFromWatcher(NewFakeWatcher(1)).(*reducerStore)

	for i := 0; i < 100; i++ {
		s.notify() // would deadlock a caller if it ever blocked
	}

	// Exactly one nudge is buffered.
	select {
	case <-s.Changed():
	default:
		t.Fatal("expected one buffered nudge after notify storm")
	}
	select {
	case <-s.Changed():
		t.Fatal("expected the buffer to hold at most one nudge")
	default:
	}
}

func TestStaticStore_SetFiresChanged(t *testing.T) {
	s := NewStaticStore(Fleet{})
	if got := len(s.Snapshot().Robots); got != 0 {
		t.Fatalf("empty fleet expected, got %d robots", got)
	}

	s.Set(Fleet{Robots: []RobotView{{Name: "robot-1"}}})

	select {
	case <-s.Changed():
	default:
		t.Fatal("Set should fire a Changed nudge")
	}
	if got := s.Snapshot().Robots; len(got) != 1 || got[0].Name != "robot-1" {
		t.Fatalf("snapshot did not reflect Set: %+v", got)
	}
}

// Compile-time interface checks.
var (
	_ Store        = (*StaticStore)(nil)
	_ Store        = (*reducerStore)(nil)
	_ FleetWatcher = (*cacheWatcher)(nil)
	_ FleetWatcher = (*FakeWatcher)(nil)
)
