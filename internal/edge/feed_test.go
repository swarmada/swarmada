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

package edge

import (
	"reflect"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/audit"
)

// feedNode builds a Node serving one edge zone ("dock", robots amr-1/amr-2) and one
// non-edge zone ("bay", amr-3), with a controllable clock and a 60s grace window.
func feedNode(clk *time.Time) *Node {
	n := New(Config{
		Namespace: "ns",
		RobotZone: map[string]string{"amr-1": "dock", "amr-2": "dock", "amr-3": "bay"},
		EdgeZones: map[string]bool{"dock": true},
	}, audit.New(&audit.MemorySink{}, "v0.1.0"))
	n.now = func() time.Time { return *clk }
	n.feedGrace = 60 * time.Second
	return n
}

// A robot whose feed never arrives is flagged only after the grace window, and only
// for edge zones.
func TestMissingFeeds_NeverSeenBeyondGraceFlagged(t *testing.T) {
	clk := time.Now()
	n := feedNode(&clk)

	if got := n.MissingFeeds()["dock"]; len(got) != 0 {
		t.Fatalf("within grace: dock = %v, want empty (grace just seeded)", got)
	}

	clk = clk.Add(2 * time.Minute)
	got := n.MissingFeeds()
	if !reflect.DeepEqual(got["dock"], []string{"amr-1", "amr-2"}) {
		t.Fatalf("dock = %v, want [amr-1 amr-2]", got["dock"])
	}
	if _, ok := got["bay"]; ok {
		t.Fatal("bay is not an edge zone; it must never appear in the report")
	}
}

// A robot whose EdgeStream feed arrived is never flagged.
func TestMissingFeeds_SeenRobotNotFlagged(t *testing.T) {
	clk := time.Now()
	n := feedNode(&clk)
	n.MissingFeeds() // seed the grace window
	n.markSeen("amr-1")

	clk = clk.Add(2 * time.Minute)
	if got := n.MissingFeeds()["dock"]; !reflect.DeepEqual(got, []string{"amr-2"}) {
		t.Fatalf("dock = %v, want [amr-2] (amr-1 has a live feed)", got)
	}
}

// Once the missing feeds arrive, the report clears.
func TestMissingFeeds_RecoveryClears(t *testing.T) {
	clk := time.Now()
	n := feedNode(&clk)
	n.MissingFeeds()
	clk = clk.Add(2 * time.Minute)
	if got := n.MissingFeeds()["dock"]; len(got) != 2 {
		t.Fatalf("expected both robots flagged, got %v", got)
	}

	n.markSeen("amr-1")
	n.markSeen("amr-2")
	if got := n.MissingFeeds()["dock"]; len(got) != 0 {
		t.Fatalf("after feeds arrive dock = %v, want empty", got)
	}
}

// Robots removed from the synced config drop out of feed tracking.
func TestMissingFeeds_PrunesDepartedRobots(t *testing.T) {
	clk := time.Now()
	n := feedNode(&clk)
	n.MissingFeeds()
	clk = clk.Add(2 * time.Minute)

	// amr-2 leaves the fleet; the report must no longer mention it.
	n.SetConfig(Config{
		Namespace: "ns",
		RobotZone: map[string]string{"amr-1": "dock"},
		EdgeZones: map[string]bool{"dock": true},
	})
	if got := n.MissingFeeds()["dock"]; !reflect.DeepEqual(got, []string{"amr-1"}) {
		t.Fatalf("dock = %v, want [amr-1] after amr-2 departed", got)
	}
}
