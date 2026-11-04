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

package executorguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestDecide_WaitsOnAnUnknownHorizonForTheLongestLease(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ref := now.Add(-10 * time.Second)
	d := Decide(Input{Reference: ref, Recorders: []Recorder{{Robot: "r1"}}, Now: now, Skew: 5 * time.Second})
	if d.Dispatchable() || d.Blocked != "r1" {
		t.Fatalf("an unknown horizon must block: %+v", d)
	}
	want := ref.Add(MaxLease+5*time.Second).Sub(now) + time.Second
	if d.Wait != want {
		t.Errorf("wait %s, want %s (the longest lease plus skew from the reference)", d.Wait, want)
	}
	late := Decide(Input{Reference: ref, Recorders: []Recorder{{Robot: "r1"}}, Now: ref.Add(MaxLease + 5*time.Second), Skew: 5 * time.Second})
	if !late.Dispatchable() || len(late.Release) != 1 {
		t.Errorf("past the bound the record is released: %+v", late)
	}
}

func TestDecide_AKnownHorizonAndAConfirmedCancel(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	h := now.Add(20 * time.Second)
	d := Decide(Input{Horizon: &h, Recorders: []Recorder{{Robot: "r1"}}, Now: now, Skew: 5 * time.Second})
	if d.Dispatchable() || d.Wait != 26*time.Second {
		t.Errorf("a live horizon blocks until horizon plus skew: %+v", d)
	}
	d = Decide(Input{Horizon: &h, Recorders: []Recorder{{Robot: "r1", CancelConfirmed: true}, {Robot: "r2", Ended: true}}, Now: now})
	if !d.Dispatchable() || len(d.Release) != 2 {
		t.Errorf("a confirmed cancel and a reported end each release: %+v", d)
	}
	d = Decide(Input{Horizon: &h, Recorders: []Recorder{{Robot: "r1", CancelConfirmed: true}, {Robot: "r2"}}, Now: now})
	if d.Dispatchable() || d.Blocked != "r2" || len(d.Release) != 1 {
		t.Errorf("one confirmed, one not: release one, block on the other: %+v", d)
	}
	if d := Decide(Input{Now: now}); !d.Dispatchable() {
		t.Errorf("no recorder, no block: %+v", d)
	}
}

func TestDecide_NoReferenceCountsFromNow(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	d := Decide(Input{Recorders: []Recorder{{Robot: "r1"}}, Now: now, Skew: time.Second})
	if d.Wait != MaxLease+2*time.Second {
		t.Errorf("with no reference the bound counts from now: %s", d.Wait)
	}
}

// MaxLease is the CRD's ceiling on leaseDurationSeconds, read from the generated CRD so the
// two cannot drift apart.
func TestMaxLease_IsTheCRDCeiling(t *testing.T) {
	path := filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_swarmadaconfigs.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	re := regexp.MustCompile(`(?s)leaseDurationSeconds:.*?maximum: (\d+)`)
	m := re.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no maximum under leaseDurationSeconds in %s", path)
	}
	n, _ := strconv.Atoi(string(m[1]))
	if time.Duration(n)*time.Second != MaxLease {
		t.Errorf("MaxLease %s, the CRD ceiling is %d s", MaxLease, n)
	}
}

// A generation is above both the namespace's mark and the action's previous one, whatever the clock
// says; the seed is above every generation an action holds even when the clock reads earlier.
func TestNextGeneration_RisesWithNoClock(t *testing.T) {
	if got := NextGeneration(10, 3); got != 11 {
		t.Errorf("NextGeneration(10, 3) = %d, want 11", got)
	}
	if got := NextGeneration(10, 30); got != 31 {
		t.Errorf("NextGeneration(10, 30) = %d, want 31", got)
	}
	stepBack := time.UnixMicro(5)
	if got := SeedGeneration(1_000_000, stepBack); got != 1_000_000 {
		t.Errorf("SeedGeneration under a clock stepped back = %d, want the highest held generation", got)
	}
}
