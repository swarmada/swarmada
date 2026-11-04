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

package streamauth

import (
	"context"
	"errors"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// THE WIRE robot_id IS NOT THE OBJECT NAME.
//
// An adapter names a robot by the robot_id it announced. The Robot object carries that id
// in its swarmada.io/robot-id annotation, and its name is whatever the operator admitted
// it as (`swarmctl admit --name`), so the two differ for a renamed robot
// (RFC-0001 #security-fleet-adapter-authentication-robot-identity). The telemetry sink
// resolves a robot_id through the annotation; the authorizer must judge the same Robot
// the message will be applied to, or it authorizes one robot and the sink writes another.

func robotWithID(name, robotID, adapter, class string) *fleetv1.Robot {
	r := robot(name, adapter, class)
	r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: robotID}
	return r
}

// Admitted under its own id (the default): name and robot_id agree.
func TestAuthorizeRobot_NameEqualsRobotID(t *testing.T) {
	a := newAuthorizer(t, robotWithID("amr-1", "amr-1", "acme", "acme-origin"), adapter("acme", "acme-origin"))
	if err := a.AuthorizeRobot(context.Background(), verified("acme"), "amr-1"); err != nil {
		t.Fatalf("a robot admitted under its own id was denied: %v", err)
	}
}

// Admitted under another name: the adapter's messages carry the robot_id, and they are
// authorized; a message naming the object instead is not a robot this adapter announced.
func TestAuthorizeRobot_RenamedRobot(t *testing.T) {
	a := newAuthorizer(t, robotWithID("picker-west", "amr-7", "acme", "acme-origin"), adapter("acme", "acme-origin"))
	if err := a.AuthorizeRobot(context.Background(), verified("acme"), "amr-7"); err != nil {
		t.Fatalf("the renamed robot's own robot_id was denied: %v", err)
	}
	err := a.AuthorizeRobot(context.Background(), verified("acme"), "picker-west")
	if !errors.Is(err, controlstream.ErrRobotNotAdmitted) {
		t.Fatalf("the object name is not a wire robot_id; want not-admitted, got %v", err)
	}
}

// A robot object whose NAME is another robot's robot_id. Robot "amr-a" (robot_id amr-b)
// is bound to adapter beta; robot "picker-x" (robot_id amr-a) is bound to adapter alpha.
// A message on robot_id amr-a is about alpha's robot, whatever object is named amr-a.
func TestAuthorizeRobot_StreamClaimingAnotherRobotsID(t *testing.T) {
	a := newAuthorizer(t,
		robotWithID("picker-x", "amr-a", "alpha", "acme-origin"),
		robotWithID("amr-a", "amr-b", "beta", "acme-origin"),
		adapter("alpha", "acme-origin"), adapter("beta", "acme-origin"))
	ctx := context.Background()

	if err := a.AuthorizeRobot(ctx, verified("beta"), "amr-a"); err == nil {
		t.Fatal("adapter beta was authorized on robot_id amr-a, which belongs to alpha's robot")
	} else if errors.Is(err, controlstream.ErrRobotNotAdmitted) {
		t.Fatalf("a claim on another adapter's robot is a denial, not onboarding: %v", err)
	}
	if err := a.AuthorizeRobot(ctx, verified("alpha"), "amr-a"); err != nil {
		t.Fatalf("alpha was denied on its own robot's robot_id: %v", err)
	}
	if err := a.AuthorizeRobot(ctx, verified("beta"), "amr-b"); err != nil {
		t.Fatalf("beta was denied on its own robot's robot_id: %v", err)
	}
}

// A first announce of a robot_id that is already admitted under another name, by a
// different adapter, is refused like any other announce of a bound robot.
func TestAuthorizeAnnounce_RenamedRobotIsBound(t *testing.T) {
	a := newAuthorizer(t, robotWithID("picker-x", "amr-a", "alpha", "acme-origin"))
	if err := a.AuthorizeAnnounce(context.Background(), verified("beta"), "amr-a"); err == nil {
		t.Fatal("beta may not announce robot_id amr-a: it is admitted, as picker-x, to alpha")
	}
	if err := a.AuthorizeAnnounce(context.Background(), verified("alpha"), "amr-a"); err != nil {
		t.Fatalf("alpha's re-announce of its own robot was refused: %v", err)
	}
}

// Two Robots answering to one robot_id cannot be told apart, so neither is authorized.
// The Robot webhook refuses a duplicate annotation; a Robot stored without one answers to
// its name, and that is the collision left.
func TestAuthorizeRobot_AmbiguousRobotIDDenied(t *testing.T) {
	a := newAuthorizer(t,
		robot("amr-a", "alpha", "acme-origin"),
		robotWithID("picker-x", "amr-a", "alpha", "acme-origin"),
		adapter("alpha", "acme-origin"))
	err := a.AuthorizeRobot(context.Background(), verified("alpha"), "amr-a")
	if err == nil || errors.Is(err, controlstream.ErrRobotNotAdmitted) {
		t.Fatalf("an ambiguous robot_id must be denied, got %v", err)
	}
}
