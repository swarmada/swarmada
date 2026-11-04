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

package webhook

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// adapterWithSimulated builds a FleetAdapter carrying the ADR-0038 declaration.
func adapterWithSimulated(ns, name string, simulated bool) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.FleetAdapterSpec{Vendor: "acme-robotics", Simulated: simulated},
	}
}

// A Robot bound to a simulated adapter is stamped Simulated (ADR-0038 §2).
func TestStampRobotSource_Simulated(t *testing.T) {
	d := coordsDefaulter(t, adapterWithSimulated("ns1", "sim-adapter", true))
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
	}

	d.stampRobotSource(context.Background(), robot)

	if got := robot.Annotations[annRobotSource]; got != string(fleetv1.RobotSourceSimulated) {
		t.Errorf("robot-source = %q, want %q", got, fleetv1.RobotSourceSimulated)
	}
}

// An adapter that does not declare itself is physical: spec.simulated defaults to
// false, and an undeclared adapter means exactly what it means today (ADR-0038 §1).
func TestStampRobotSource_Physical(t *testing.T) {
	d := coordsDefaulter(t, adapterWithSimulated("ns1", "hw-adapter", false))
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "hw-adapter"}},
	}

	d.stampRobotSource(context.Background(), robot)

	if got := robot.Annotations[annRobotSource]; got != string(fleetv1.RobotSourcePhysical) {
		t.Errorf("robot-source = %q, want %q", got, fleetv1.RobotSourcePhysical)
	}
}

// Fail-safe (ADR-0038 §2): an unresolvable adapter leaves the annotation UNSET —
// never a guessed value. Absent means "unknown", not "assumed physical".
func TestStampRobotSource_UnresolvableLeavesUnset(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adapterName string
	}{
		{"unset adapter name", ""},
		{"adapter not found", "missing-adapter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := coordsDefaulter(t) // no FleetAdapter objects
			robot := &fleetv1.Robot{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"},
				Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: tc.adapterName}},
			}

			d.stampRobotSource(context.Background(), robot)

			if got, ok := robot.Annotations[annRobotSource]; ok {
				t.Errorf("robot-source = %q, want the annotation to be absent", got)
			}
		})
	}
}

// Default stamps the source alongside the ADR-0017 coordinate stamps, for a Robot
// with no RobotClass (the stamp does not depend on the class merge).
func TestDefault_StampsRobotSource(t *testing.T) {
	d := coordsDefaulter(t, adapterWithSimulated("ns1", "sim-adapter", true))
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
	}

	if err := d.Default(context.Background(), robot); err != nil {
		t.Fatalf("Default: %v", err)
	}

	if got := robot.Annotations[annRobotSource]; got != string(fleetv1.RobotSourceSimulated) {
		t.Errorf("robot-source = %q, want %q", got, fleetv1.RobotSourceSimulated)
	}
}
