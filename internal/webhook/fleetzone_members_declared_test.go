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

// A robot is never in a composite (RFC-0001 FZ-C2). The Robot gate refuses a composite
// spec.zone when the robot is written; the FleetZone validator refuses the zone becoming
// a composite under robots that already declare it. Without the second, those robots
// kept the zone, FZ-G5 placed them in it, and a zone estop on it was refused (FZ-C5).

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// declaredWorld is a fake API server holding zones and the robots that declare them.
func declaredWorld(t *testing.T, zones []*fleetv1.FleetZone, declares map[string]string) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, len(zones)+len(declares))
	for _, z := range zones {
		objs = append(objs, z.DeepCopy())
	}
	for robot, zone := range declares {
		objs = append(objs, &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: robot, Namespace: "ns"},
			Spec:       fleetv1.RobotSpec{Zone: zone},
		})
	}
	return fake.NewClientBuilder().WithScheme(fzrScheme(t)).WithObjects(objs...).Build()
}

func withMembers(z *fleetv1.FleetZone, members ...string) *fleetv1.FleetZone {
	c := z.DeepCopy()
	c.Spec.MemberZones = members
	return c
}

func TestFleetZoneMembersAreRefusedOnAZoneARobotDeclares(t *testing.T) {
	ward := fzrZone("ward-2", "", nil)
	roomA := fzrZone("room-a", "", nil)
	c := declaredWorld(t, []*fleetv1.FleetZone{ward, roomA},
		map[string]string{"r-2": "ward-2", "r-1": "ward-2", "r-a": "room-a"})
	v := &FleetZoneValidator{Client: c, APIReader: c}
	ctx := context.Background()

	t.Run("members added to a zone robots declare are refused", func(t *testing.T) {
		_, err := v.ValidateUpdate(ctx, ward, withMembers(ward, "room-a"))
		if err == nil {
			t.Fatal("ward-2, declared by r-1 and r-2, was ADMITTED as a composite: those robots would " +
				"be in a composite, where no zone estop reaches them")
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("want Invalid on spec.memberZones, got %v", err)
		}
		for _, want := range []string{"spec.memberZones", "[r-1 r-2]", "re-home them"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}
	})

	t.Run("a zone no robot declares may become a composite", func(t *testing.T) {
		empty := fzrZone("ward-3", "", nil)
		cc := declaredWorld(t, []*fleetv1.FleetZone{empty, roomA}, map[string]string{"r-a": "room-a"})
		vv := &FleetZoneValidator{Client: cc, APIReader: cc}
		if _, err := vv.ValidateUpdate(ctx, empty, withMembers(empty, "room-a")); err != nil {
			t.Fatalf("ward-3 is declared by no robot: %v", err)
		}
	})

	t.Run("an existing composite's member list may be edited", func(t *testing.T) {
		// Already a composite (stored before this rule, or through a concurrent write):
		// editing its members makes nothing worse, and refusing it would pin the list.
		old := withMembers(ward, "room-a")
		if _, err := v.ValidateUpdate(ctx, old, withMembers(ward, "room-a", "room-b")); err != nil {
			t.Fatalf("editing an existing composite's members was refused: %v", err)
		}
	})

	t.Run("without an uncached reader it fails closed", func(t *testing.T) {
		vv := &FleetZoneValidator{Client: c}
		if _, err := vv.ValidateUpdate(ctx, ward, withMembers(ward, "room-a")); err == nil {
			t.Fatal("members were admitted with no reader to check which robots declare the zone")
		}
	})
}
