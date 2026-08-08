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
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

type admitCall struct {
	robot string
	zone  string
	admit bool
}

type fakeZoneAdmit struct {
	mu    sync.Mutex
	calls []admitCall
}

func (f *fakeZoneAdmit) NotifyZoneAdmission(_ context.Context, _, robotID, zoneName string, admit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, admitCall{robotID, zoneName, admit})
}

func (f *fakeZoneAdmit) has(c admitCall) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.calls {
		if got == c {
			return true
		}
	}
	return false
}

func zoneWithCapacity(name string, maxRobots int32) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetZoneSpec{MaxConcurrentRobots: maxRobots},
	}
}

// §9.3.4: a robot entering a leaf zone already at maxConcurrentRobots is HELD at the
// boundary (zone_admission admit=false); when a slot frees (another robot leaves),
// the held robot is ADMITTED (admit=true). Enforcement is advisory — the control
// plane pushes; the adapter honours.
func TestZoneObserver_HoldsOverCapacityAndAdmitsOnExit(t *testing.T) {
	r1 := robotAt("r1", "rid1", "")
	r2 := robotAt("r2", "rid2", "")
	notifier := &fakeZoneAdmit{}
	z, _ := newZoneController(t, zoneWithCapacity("z", 1), r1, r2)
	z.Admission = notifier
	z.setZones(actionNS, []zone.Candidate{{Name: "z", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})

	inside := &telemetry.Position{X: 60, Y: 40, Floor: 0}
	outside := &telemetry.Position{X: 10000, Y: 10000, Floor: 0}
	ctx := context.Background()

	// r1 enters z (occupancy 1, at capacity but not over) → no hold.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	if notifier.has(admitCall{"r1", "z", false}) {
		t.Fatal("r1 must NOT be held: it is the 1st robot in a max=1 zone")
	}

	// r2 enters z (occupancy 2 > max=1) → r2 held at the boundary.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid2", Position: inside})
	if !notifier.has(admitCall{"r2", "z", false}) {
		t.Fatal("r2 must be HELD (admit=false) — it exceeds maxConcurrentRobots")
	}

	// r1 leaves z → a slot frees → r2 admitted.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: outside})
	if !notifier.has(admitCall{"r2", "z", true}) {
		t.Fatal("r2 must be ADMITTED (admit=true) once a slot frees")
	}
}

// With no maxConcurrentRobots (unlimited), no robot is ever held.
func TestZoneObserver_UnlimitedZoneNeverHolds(t *testing.T) {
	r1 := robotAt("r1", "rid1", "")
	r2 := robotAt("r2", "rid2", "")
	notifier := &fakeZoneAdmit{}
	z, _ := newZoneController(t, zoneWithCapacity("z", 0), r1, r2) // 0 = unlimited
	z.Admission = notifier
	z.setZones(actionNS, []zone.Candidate{{Name: "z", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})

	inside := &telemetry.Position{X: 60, Y: 40, Floor: 0}
	ctx := context.Background()
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid2", Position: inside})

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 0 {
		t.Fatalf("unlimited zone must never push zone_admission, got %v", notifier.calls)
	}
}
