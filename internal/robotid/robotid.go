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

// Package robotid maps between a Robot object and the robot_id its Fleet Adapter knows it
// by. The two are different names for one robot: the object name is what the control
// plane uses internally (controllers, status, events, metrics), and the robot_id is what
// the adapter announced and what goes on the wire, in both directions
// (RFC-0001 #security-fleet-adapter-authentication-robot-identity). They differ whenever
// an operator admits a robot under another name (`swarmctl admit --name`).
package robotid

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Wire is the robot_id robot's adapter knows it by: the swarmada.io/robot-id annotation.
// A Robot stored without the annotation falls back to its name. The Robot defaulting
// webhook stamps the annotation with the name on every write that lacks it, so only a
// Robot stored before that webhook existed can reach the fallback, and for it the name is
// the id by construction.
func Wire(robot *fleetv1.Robot) string {
	if id := robot.Annotations[fleetv1.RobotIDAnnotation]; id != "" {
		return id
	}
	return robot.Name
}

// ErrUnreadable is in the chain of a Lookup error that comes from reading the Robots, not from
// what they hold: a transient failure, after which the same lookup may succeed. A robot_id that
// names more than one Robot is not such an error.
var ErrUnreadable = errors.New("the Robots could not be read")

// Lookup resolves a wire robot_id to the Robot in namespace that Wire maps to it. It
// returns nil and no error when no Robot does. More than one match is an error, because
// nothing says which is meant: the Robot webhook refuses a duplicate annotation, so the
// collision left is a Robot stored without one whose name is another Robot's robot_id.
func Lookup(ctx context.Context, c client.Reader, namespace, robotID string) (*fleetv1.Robot, error) {
	var robots fleetv1.RobotList
	if err := c.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("robot_id %q lookup failed: %w: %w", robotID, ErrUnreadable, err)
	}
	var match *fleetv1.Robot
	for i := range robots.Items {
		r := &robots.Items[i]
		if Wire(r) != robotID {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("robot_id %q names more than one Robot in namespace %q (%s, %s)",
				robotID, namespace, match.Name, r.Name)
		}
		match = r
	}
	return match, nil
}
