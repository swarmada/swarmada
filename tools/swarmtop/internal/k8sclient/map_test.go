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

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

func i32(v int32) *int32 { return &v }

func TestMapRobot_CoreFields(t *testing.T) {
	seen := metav1.NewTime(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	no := false
	r := &swarmadav1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "robot-3"},
		Spec: swarmadav1.RobotSpec{
			Zone:    "B",
			Adapter: swarmadav1.AdapterRef{Name: "vda5050-a", Version: "1.0.0"},
		},
		Status: swarmadav1.RobotStatus{
			Phase:                  swarmadav1.RobotPhaseInProgress,
			BatteryPercent:         i32(23),
			CurrentZone:            "B",
			SpecZoneMatchesCurrent: &no,
			AssignedAction:         "haul-8846",
			Position:               &swarmadav1.RobotPosition{X: 14.2, Y: 8.7, Floor: i32(2)},
			Connectivity:           &swarmadav1.ConnectivityStatus{LastSeenAt: &seen},
		},
	}

	v := mapRobot(r)

	if v.Name != "robot-3" || v.Phase != "InProgress" {
		t.Fatalf("name/phase: got %q/%q", v.Name, v.Phase)
	}
	if v.BatteryPercent == nil || *v.BatteryPercent != 23 {
		t.Fatalf("battery: got %v", v.BatteryPercent)
	}
	if v.AdapterName != "vda5050-a" {
		t.Fatalf("adapter: got %q", v.AdapterName)
	}
	if !v.ZoneDrift {
		t.Fatalf("expected zone drift when specZoneMatchesCurrent is false")
	}
	if !v.HasPosition || v.Position.X != 14.2 || v.Position.Floor == nil || *v.Position.Floor != 2 {
		t.Fatalf("position: got %+v", v.Position)
	}
	if v.TelemetryUnknown || !v.LastTelemetry.Equal(seen.Time) {
		t.Fatalf("telemetry: unknown=%v last=%v", v.TelemetryUnknown, v.LastTelemetry)
	}
	if v.Estop != "Normal" {
		t.Fatalf("empty estop should normalize to Normal, got %q", v.Estop)
	}
}

func TestMapRobot_UnreportedOptionals(t *testing.T) {
	r := &swarmadav1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "robot-4"},
		Status:     swarmadav1.RobotStatus{Phase: swarmadav1.RobotPhaseOffline},
	}
	v := mapRobot(r)
	if v.BatteryPercent != nil {
		t.Fatalf("battery should stay nil when unreported")
	}
	if v.HasPosition {
		t.Fatalf("HasPosition should be false when position is nil")
	}
	if !v.TelemetryUnknown {
		t.Fatalf("TelemetryUnknown should be true when connectivity is nil")
	}
	if v.ZoneDrift {
		t.Fatalf("nil specZoneMatchesCurrent must not read as drift")
	}
}

func TestMapRobot_PositionCopyIsIndependent(t *testing.T) {
	floor := int32(2)
	r := &swarmadav1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "robot-1"},
		Status:     swarmadav1.RobotStatus{Position: &swarmadav1.RobotPosition{Floor: &floor}},
	}
	v := mapRobot(r)
	floor = 9 // mutate the source
	if v.Position.Floor == nil || *v.Position.Floor != 2 {
		t.Fatalf("view must hold an independent copy of Floor, got %v", v.Position.Floor)
	}
}

func TestMapCapabilities_Summary(t *testing.T) {
	cases := []struct {
		name        string
		in          []swarmadav1.CapabilityStatusEntry
		wantActive  int
		wantTotal   int
		wantProblem string
		wantProbSt  string
	}{
		{
			name: "all active",
			in: []swarmadav1.CapabilityStatusEntry{
				{Name: "lift", Status: swarmadav1.CapabilityStatusActive},
				{Name: "nav", Status: swarmadav1.CapabilityStatusActive},
			},
			wantActive: 2, wantTotal: 2, wantProblem: "", wantProbSt: "",
		},
		{
			name: "first non-active is the headline",
			in: []swarmadav1.CapabilityStatusEntry{
				{Name: "lift", Status: swarmadav1.CapabilityStatusActive},
				{Name: "nav", Status: swarmadav1.CapabilityStatusActive},
				{Name: "cam_front", Status: swarmadav1.CapabilityStatusDegraded, Reason: "hardware fault"},
			},
			wantActive: 2, wantTotal: 3, wantProblem: "cam_front", wantProbSt: "Degraded",
		},
		{
			name:       "empty",
			in:         nil,
			wantActive: 0, wantTotal: 0, wantProblem: "", wantProbSt: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum, views := mapCapabilities(tc.in)
			if sum.Active != tc.wantActive || sum.Total != tc.wantTotal {
				t.Fatalf("counts: got %d/%d want %d/%d", sum.Active, sum.Total, tc.wantActive, tc.wantTotal)
			}
			if sum.FirstProblem != tc.wantProblem || sum.FirstProblemState != tc.wantProbSt {
				t.Fatalf("problem: got %q/%q want %q/%q", sum.FirstProblem, sum.FirstProblemState, tc.wantProblem, tc.wantProbSt)
			}
			if len(views) != len(tc.in) {
				t.Fatalf("views len: got %d want %d", len(views), len(tc.in))
			}
		})
	}
}

func TestMapFleetAction(t *testing.T) {
	deadline := metav1.NewTime(time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC))
	action := &swarmadav1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "haul-8846"},
		Spec:       swarmadav1.FleetActionSpec{Priority: swarmadav1.ActionPriorityHigh, Deadline: &deadline},
		Status: swarmadav1.FleetActionStatus{
			Phase:         swarmadav1.ActionPhase("InProgress"),
			AssignedRobot: "robot-3",
			ProgressPct:   62,
			RetryCount:    1,
		},
	}
	v := mapFleetAction(action)
	if v.Name != "haul-8846" || v.AssignedRobot != "robot-3" || v.Priority != "High" {
		t.Fatalf("core: %+v", v)
	}
	if v.ProgressPct != 62 || v.RetryCount != 1 {
		t.Fatalf("progress/retry: %+v", v)
	}
	if v.Deadline == nil || !v.Deadline.Equal(deadline.Time) {
		t.Fatalf("deadline: %v", v.Deadline)
	}
}

func TestMapFleetAdapter(t *testing.T) {
	hb := metav1.NewTime(time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC))
	a := &swarmadav1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "vda5050-a"},
		Status: swarmadav1.FleetAdapterStatus{
			Phase:                     swarmadav1.FleetAdapterPhase("Connected"),
			Conformance:               swarmadav1.ConformanceState("Passed"),
			NegotiatedProtocolVersion: "1.0.0",
			ConnectedRobots:           3,
			LastHeartbeat:             &hb,
		},
	}
	v := mapFleetAdapter(a)
	if v.Name != "vda5050-a" || v.Conformance != "Passed" || v.ConnectedRobots != 3 {
		t.Fatalf("core: %+v", v)
	}
	if v.HeartbeatUnknown || !v.LastHeartbeat.Equal(hb.Time) {
		t.Fatalf("heartbeat: unknown=%v last=%v", v.HeartbeatUnknown, v.LastHeartbeat)
	}
}

// TestMapFleetZone pins the projection, including the two cases that are easy to get wrong:
// an empty estopStatus means Clear (per the API), and an absent lastEstopAt must be reported as
// unknown rather than as the zero time.
func TestMapFleetZone(t *testing.T) {
	z := &swarmadav1.FleetZone{}
	z.Name = "yard"
	z.Spec.DisplayName = "Outer yard"
	z.Spec.MaxConcurrentRobots = 4
	z.Status.RobotCount = 5
	z.Status.CurrentConcurrentRobots = 3
	z.Status.ChildZones = []string{"dock-a"}
	z.Status.EdgeFeedUnavailable = []string{"amr-1"}

	v := mapFleetZone(z)
	if v.Name != "yard" || v.DisplayName != "Outer yard" {
		t.Fatalf("identity: %+v", v)
	}
	if v.EstopStatus != "Clear" {
		t.Fatalf("empty estopStatus must normalise to Clear, got %q", v.EstopStatus)
	}
	if !v.LastEstopUnknown || !v.LastEstopAt.IsZero() {
		t.Fatalf("absent lastEstopAt must be reported unknown, got %+v", v)
	}
	if v.RobotCount != 5 || v.CurrentConcurrent != 3 || v.MaxConcurrentRobots != 4 {
		t.Fatalf("counts: %+v", v)
	}
	if len(v.ChildZones) != 1 || len(v.EdgeFeedUnavailable) != 1 {
		t.Fatalf("slices: %+v", v)
	}
}
