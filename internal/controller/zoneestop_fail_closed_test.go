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
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// zoneListFails is a reader whose FleetZone list fails, as the API server can.
type zoneListFails struct{ client.Reader }

func (z zoneListFails) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*fleetv1.FleetZoneList); ok {
		return errors.New("injected: the zone list could not be read")
	}
	return z.Reader.List(ctx, list, opts...)
}

// A fan-out that cannot read the zone tree must not read a robot as "not covered": the pass
// fails and is retried, so the stop is never shed on a read error.
func TestZoneEstop_RobotsInScopeFailClosedOnAReadError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zones := []client.Object{
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z0", Namespace: "ns"}},
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z1", Namespace: "ns"}, Spec: fleetv1.FleetZoneSpec{ParentZone: "z0"}},
	}
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns"}, Spec: fleetv1.RobotSpec{Zone: "z1"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(zones, robot)...).Build()

	healthy := &ZoneEstopReconciler{Client: c, Scheme: scheme, APIReader: c}
	in, err := healthy.robotsInEstopScope(context.Background(), "ns", "z0", true)
	if err != nil || len(in) != 1 {
		t.Fatalf("with a readable tree z0's stop reaches r1: %v %v", in, err)
	}

	failing := &ZoneEstopReconciler{Client: c, Scheme: scheme, APIReader: zoneListFails{Reader: c}}
	if _, err := failing.robotsInEstopScope(context.Background(), "ns", "z0", true); err == nil {
		t.Errorf("a zone list that cannot be read must fail the pass, not read as 'not covered'")
	}

	// A chain deeper than the bound (two zone edits admitted together can leave one) does not
	// fail the pass: a robot whose chain cannot be walked is in scope, so the stop stops it, and
	// every other robot the stop reaches is stopped with it. Failing the pass would stop none.
	deep := []client.Object{robot}
	for i := 0; i <= 65; i++ {
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("d%d", i), Namespace: "ns"}}
		if i > 0 {
			z.Spec.ParentZone = fmt.Sprintf("d%d", i-1)
		}
		deep = append(deep, z)
	}
	robot.Spec.Zone = "d65"
	near := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "ns"}, Spec: fleetv1.RobotSpec{Zone: "d3"}}
	elsewhere := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r3", Namespace: "ns"}, Spec: fleetv1.RobotSpec{Zone: "e0"}}
	deep = append(deep, near, elsewhere,
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "e0", Namespace: "ns"}})
	dc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deep...).Build()
	r := &ZoneEstopReconciler{Client: dc, Scheme: scheme, APIReader: dc}
	in, err = r.robotsInEstopScope(context.Background(), "ns", "d0", true)
	if err != nil {
		t.Fatalf("a robot whose chain cannot be walked failed the whole pass, so the stop reaches no robot: %v", err)
	}
	got := map[string]bool{}
	for _, rb := range in {
		got[rb.Name] = true
	}
	if !got["r1"] {
		t.Errorf("a robot 65 levels below the stopped zone cannot be placed; the stop must stop it, not skip it (in scope: %v)", got)
	}
	if !got["r2"] {
		t.Errorf("a robot inside the bound was not stopped with the stop (in scope: %v)", got)
	}
	if got["r3"] {
		t.Errorf("a robot in another tree, whose chain is walked, was put in scope (in scope: %v)", got)
	}
}
