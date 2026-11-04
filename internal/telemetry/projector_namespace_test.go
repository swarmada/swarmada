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
	"testing"

	"github.com/swarmada/swarmada/internal/telemetry"
)

// Two namespaces may each hold a robot with the same robot_id. The projector must
// remember them separately: if it kept one entry per id, the second robot's first
// frame would be compared against the first robot's last write, found unchanged, and
// the second robot would never receive its establishing status write.
func TestProjector_SameRobotIDInTwoNamespacesIsTwoRobots(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	a := telemetry.Frame{RobotID: "chassis-7", Namespace: "staging", BatteryPct: i32(80)}
	b := telemetry.Frame{RobotID: "chassis-7", Namespace: "production", BatteryPct: i32(80)}

	upA := p.Project(a)
	if upA == nil || upA.Namespace != "staging" {
		t.Fatalf("first frame in staging: update = %+v, want the establishing write carrying namespace staging", upA)
	}
	upB := p.Project(b)
	if upB == nil || upB.Namespace != "production" {
		t.Fatalf("first frame in production: update = %+v, want its own establishing write; "+
			"the projector compared it against the staging robot's state", upB)
	}

	// RA-1 holds per robot: an identical frame for either writes nothing.
	if up := p.Project(a); up != nil {
		t.Fatalf("second identical staging frame produced %+v, want nil", up)
	}
	if up := p.Project(b); up != nil {
		t.Fatalf("second identical production frame produced %+v, want nil", up)
	}
}

// Forget with an empty namespace means the caller does not know it: the id is
// cleared under every namespace, and both robots' next frames are first sight.
func TestProjector_ForgetWithNoNamespaceClearsTheIDInEveryNamespace(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	a := telemetry.Frame{RobotID: "chassis-7", Namespace: "staging", BatteryPct: i32(80)}
	b := telemetry.Frame{RobotID: "chassis-7", Namespace: "production", BatteryPct: i32(80)}
	other := telemetry.Frame{RobotID: "chassis-8", Namespace: "staging", BatteryPct: i32(80)}
	for _, f := range []telemetry.Frame{a, b, other} {
		p.Project(f)
	}

	if !p.Forget("", "chassis-7") {
		t.Fatal("Forget reported nothing held for chassis-7")
	}
	if p.Project(a) == nil || p.Project(b) == nil {
		t.Fatal("after Forget, both namespaces' chassis-7 must re-establish on their next frame")
	}
	if up := p.Project(other); up != nil {
		t.Fatalf("Forget(chassis-7) disturbed chassis-8: %+v", up)
	}
}

// Forget with a namespace clears that namespace's robot and the entry recorded from
// a stream with no namespace, which cannot be attributed to either robot. The same
// robot_id in another namespace is another robot, and its next unchanged frame
// stays silent.
func TestProjector_ForgetWithANamespaceLeavesTheOtherNamespaceAlone(t *testing.T) {
	p := telemetry.NewProjector(telemetry.DefaultConfig())
	a := telemetry.Frame{RobotID: "chassis-7", Namespace: "staging", BatteryPct: i32(80)}
	b := telemetry.Frame{RobotID: "chassis-7", Namespace: "production", BatteryPct: i32(80)}
	unscoped := telemetry.Frame{RobotID: "chassis-7", BatteryPct: i32(80)}
	for _, f := range []telemetry.Frame{a, b, unscoped} {
		p.Project(f)
	}

	if !p.Forget("staging", "chassis-7") {
		t.Fatal("Forget reported nothing held for staging/chassis-7")
	}
	if p.Project(a) == nil {
		t.Fatal("after Forget, staging/chassis-7 must re-establish on its next frame")
	}
	if p.Project(unscoped) == nil {
		t.Fatal("after Forget, the entry with no namespace must re-establish on its next frame")
	}
	if up := p.Project(b); up != nil {
		t.Fatalf("Forget(staging, chassis-7) disturbed production/chassis-7: %+v", up)
	}
}
