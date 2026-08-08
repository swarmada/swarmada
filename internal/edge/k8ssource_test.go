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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func edgeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fzWithBounds(ns, name string, floor int32, poly []fleetv1.Point) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{Floor: floor, Polygon: poly}},
	}
}

func robotInZone(ns, name, zone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.RobotSpec{Zone: zone},
	}
}

func squarePoints() []fleetv1.Point {
	return []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
}

func TestKubeConfigSource_BuildsConfigFromFleetZonesAndRobots(t *testing.T) {
	sc := edgeScheme(t)
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(
		fzWithBounds("warehouse-a", "zone-a", 1, squarePoints()),
		robotInZone("warehouse-a", "amr-1", "zone-a"),
		robotInZone("warehouse-a", "amr-2", "zone-a"),
		robotInZone("other-ns", "amr-x", "zone-a"), // different namespace: excluded
	).Build()

	cfg, err := NewKubeConfigSource(c, "warehouse-a").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Namespace != "warehouse-a" {
		t.Errorf("namespace = %q", cfg.Namespace)
	}
	if len(cfg.Zones) != 1 || cfg.Zones[0].Name != "zone-a" || cfg.Zones[0].Floor != 1 {
		t.Fatalf("zones = %+v", cfg.Zones)
	}
	if len(cfg.Zones[0].Polygon) != 4 {
		t.Errorf("polygon vertices = %d, want 4", len(cfg.Zones[0].Polygon))
	}
	if cfg.RobotZone["amr-1"] != "zone-a" || cfg.RobotZone["amr-2"] != "zone-a" {
		t.Errorf("robot assignments = %+v", cfg.RobotZone)
	}
	if _, leaked := cfg.RobotZone["amr-x"]; leaked {
		t.Error("a robot from another namespace leaked into the config")
	}
}

func TestKubeConfigSource_SkipsZonesWithoutBoundsAndUnboundedRobots(t *testing.T) {
	sc := edgeScheme(t)
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(
		fzWithBounds("warehouse-a", "zone-a", 0, squarePoints()),
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: "warehouse-a", Name: "no-bounds"}}, // no physicalBounds
		robotInZone("warehouse-a", "amr-1", "zone-a"),
		robotInZone("warehouse-a", "amr-2", "no-bounds"), // assigned to an unevaluable zone
	).Build()

	cfg, err := NewKubeConfigSource(c, "warehouse-a").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Zones) != 1 || cfg.Zones[0].Name != "zone-a" {
		t.Fatalf("zones = %+v, want only the bounded zone-a", cfg.Zones)
	}
	// A robot assigned to a zone the node cannot evaluate is omitted — the node never
	// acts on a zone it has no polygon for.
	if _, ok := cfg.RobotZone["amr-2"]; ok {
		t.Error("robot assigned to an unbounded zone should be omitted")
	}
	if cfg.RobotZone["amr-1"] != "zone-a" {
		t.Errorf("robot assignments = %+v", cfg.RobotZone)
	}
}
