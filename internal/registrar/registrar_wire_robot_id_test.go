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

package registrar

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// REGISTER AND DISCOVER RESOLVE THE ROBOT_ID THE ADAPTER ANNOUNCED.
//
// An adapter names a robot by the robot_id it announced; the Robot carries it in its
// swarmada.io/robot-id annotation, and its name is whatever the operator admitted it as
// (`swarmctl admit --name`). A name lookup finds no Robot for a renamed robot, so its
// Register is refused NOT_ADMITTED for ever, and the Discover that follows stages a
// DiscoveredRobot for a robot that is already admitted.

func robotNamed(name, robotID string) *fleetv1.Robot {
	interval := int32(7)
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: name,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: robotID}},
		Spec: fleetv1.RobotSpec{Zone: "dock-1", TelemetryIntervalSeconds: &interval},
	}
}

// reconnect is what a conformant adapter does on every (re)connect (C2.1): Register, and
// announce with Discover only when the control plane answers NOT_ADMITTED.
func reconnect(r *Registrar, robotID string) (*fav1.RegisterAck, *fav1.DiscoverAck) {
	ctx := context.Background()
	reg := r.Register(ctx, adapterID(), &fav1.RegisterRobot{RobotId: robotID})
	if reg.GetAccepted() || reg.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		return reg, nil
	}
	return reg, r.Discover(ctx, adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: robotID})
}

func discoveredRobots(t *testing.T, c client.Client) int {
	t.Helper()
	list := &fleetv1.DiscoveredRobotList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

// Both admission paths: the admitted robot re-registers on reconnect, gets its RegisterAck,
// and no DiscoveredRobot is staged for it.
func TestRegistrarWireID_AdmittedRobotReRegistersOnReconnect(t *testing.T) {
	for _, tc := range []struct{ desc, name, wireID string }{
		{"admitted under its own id", "amr-07", "amr-07"},
		{"admitted under another name", "picker-west", "amr-07"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			r, c := newRegistrar(t, robotNamed(tc.name, tc.wireID))
			reg, disc := reconnect(r, tc.wireID)
			if !reg.GetAccepted() {
				t.Fatalf("Register(%s) = %+v; the admitted Robot %s was not found by its robot_id", tc.wireID, reg, tc.name)
			}
			if reg.GetTelemetryIntervalSeconds() != 7 || reg.GetAssignedZone() != "dock-1" {
				t.Errorf("RegisterAck = %+v, want the Robot's interval 7 and zone dock-1", reg)
			}
			if disc != nil {
				t.Errorf("the adapter was sent to Discover: %+v", disc)
			}
			if n := discoveredRobots(t, c); n != 0 {
				t.Errorf("%d DiscoveredRobots staged for an admitted robot, want 0", n)
			}
		})
	}
}

// A Discover naming an admitted robot's robot_id is answered ALREADY_EXISTS, and stages
// nothing, whatever the Robot is named.
func TestRegistrarWireID_DiscoverOfAnAdmittedRobotStagesNothing(t *testing.T) {
	for _, tc := range []struct{ desc, name, wireID string }{
		{"admitted under its own id", "amr-07", "amr-07"},
		{"admitted under another name", "picker-west", "amr-07"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			r, c := newRegistrar(t, robotNamed(tc.name, tc.wireID))
			ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: tc.wireID})
			if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_ALREADY_EXISTS {
				t.Errorf("Discover(%s) = %+v, want ALREADY_EXISTS", tc.wireID, ack)
			}
			if n := discoveredRobots(t, c); n != 0 {
				t.Errorf("%d DiscoveredRobots staged for an admitted robot, want 0", n)
			}
		})
	}
}

// The object name of a renamed robot is not a robot_id any adapter announced.
func TestRegistrarWireID_ObjectNameIsNotARobotID(t *testing.T) {
	r, _ := newRegistrar(t, robotNamed("picker-west", "amr-07"))
	ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: "picker-west"})
	if ack.GetAccepted() || ack.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		t.Fatalf("Register(picker-west) = %+v, want NOT_ADMITTED: that is the object name, not a robot_id", ack)
	}
}

// Two Robots answering to one robot_id cannot be told apart: Register is not accepted,
// and Discover stages nothing.
func TestRegistrarWireID_AmbiguousRobotIDIsRefused(t *testing.T) {
	legacy := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "amr-07"}}
	r, c := newRegistrar(t, legacy, robotNamed("picker-west", "amr-07"))
	if ack := r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: "amr-07"}); ack.GetAccepted() {
		t.Errorf("Register of an ambiguous robot_id was accepted: %+v", ack)
	}
	if ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{}, &fav1.DiscoverRobot{RobotId: "amr-07"}); ack.GetAccepted() {
		t.Errorf("Discover of an ambiguous robot_id was accepted: %+v", ack)
	}
	if n := discoveredRobots(t, c); n != 0 {
		t.Errorf("%d DiscoveredRobots staged for an ambiguous robot_id, want 0", n)
	}
}
