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

// Package streamauth is the Kubernetes-backed per-message robot_id authorizer for
// the Fleet Adapter ControlStream/SafetyStream (RFC-0001 #security-fleet-adapter-authentication-robot-identity, #fleet-adapter-protocol-transport-security). It
// resolves the adapter's mTLS identity to the robots it may act on and fails
// closed on any error, absence, or mismatch.
package streamauth

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/robotid"
)

// Authorizer authorizes adapter messages against live Robot / FleetAdapter state.
// It implements controlstream.Authorizer and is safe for concurrent use (the
// underlying client is). Every decision fails closed.
type Authorizer struct {
	// Client reads Robot and FleetAdapter resources; a cached reader is fine.
	Client client.Reader
}

// AuthorizeRobot permits a message on an admitted robot only when the robot exists
// in the adapter's namespace, its spec.adapter names this adapter, and its
// RobotClass is one the adapter declares it serves (RFC-0001 #fleet-adapter-protocol-transport-security). Anything else — an
// unverified identity, a missing robot, a different owner, an unserved class, or a
// lookup error — is denied. The adapter is read last. Its denial wraps
// controlstream.ErrClassNotServed for an unserved class and controlstream.ErrAdapterNotFound
// for a FleetAdapter that does not exist, so the stream can tell those from the others: either
// alone does not refuse a message that observes or stops the robot.
//
// robotID is the wire robot_id the message names, and it is resolved to a Robot through
// the swarmada.io/robot-id annotation (robotid.Lookup), the resolution the telemetry sink
// applies to the same message, so the Robot authorized is the Robot written. The object
// name is not the id: for a robot admitted under another name (`swarmctl admit --name`)
// the two differ, and a name lookup would judge whichever object happens to carry the
// name. A robot_id that names more than one Robot is denied.
func (a *Authorizer) AuthorizeRobot(ctx context.Context, adapter controlstream.TLSIdentity, robotID string) error {
	if !adapter.Verified {
		return fmt.Errorf("unauthenticated adapter: no verified mTLS identity")
	}
	if robotID == "" {
		return fmt.Errorf("empty robot_id")
	}

	robot, err := robotid.Lookup(ctx, a.Client, adapter.Namespace, robotID)
	if err != nil {
		return err // fail closed on transient errors and ambiguity
	}
	if robot == nil {
		// Wrapped so the ControlStream can tell "not admitted yet" from a real
		// authorization violation: the first routes the adapter into the discovery
		// flow, the second must stay opaque.
		return fmt.Errorf("robot %q not found in namespace %q: %w",
			robotID, adapter.Namespace, controlstream.ErrRobotNotAdmitted)
	}
	if robot.Spec.Adapter.Name != adapter.AdapterName {
		return fmt.Errorf("robot %q is bound to adapter %q, not %q", robotID, robot.Spec.Adapter.Name, adapter.AdapterName)
	}
	return a.authorizeClass(ctx, adapter, robot.Spec.RobotClass)
}

// AuthorizeAnnounce permits a first-time Discover unless the robot_id is already
// bound to a different adapter in the namespace. An unverified identity is denied. The
// robot_id is resolved as AuthorizeRobot resolves it.
func (a *Authorizer) AuthorizeAnnounce(ctx context.Context, adapter controlstream.TLSIdentity, robotID string) error {
	if !adapter.Verified {
		return fmt.Errorf("unauthenticated adapter: no verified mTLS identity")
	}
	if robotID == "" {
		return fmt.Errorf("empty robot_id")
	}

	robot, err := robotid.Lookup(ctx, a.Client, adapter.Namespace, robotID)
	switch {
	case err != nil:
		return err // fail closed
	case robot == nil:
		return nil // not yet admitted — a first announce is permitted
	case robot.Spec.Adapter.Name != adapter.AdapterName:
		return fmt.Errorf("robot %q is already bound to adapter %q", robotID, robot.Spec.Adapter.Name)
	default:
		return nil
	}
}

// authorizeClass checks that the adapter declares it serves robotClass. Only a FleetAdapter
// that was read and lists other classes wraps controlstream.ErrClassNotServed: that adapter
// fails the admission gate's class condition. A failed read does not wrap it, and neither does
// an empty list, which passes that condition (it serves every class at admission and at
// dispatch) and is refused here for every message. A read that finds no FleetAdapter wraps
// controlstream.ErrAdapterNotFound: that adapter fails the gate's existence condition. Every
// other failed read wraps neither, and fails closed.
func (a *Authorizer) authorizeClass(ctx context.Context, adapter controlstream.TLSIdentity, robotClass string) error {
	fa := &fleetv1.FleetAdapter{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: adapter.AdapterName, Namespace: adapter.Namespace}, fa); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("adapter %q not found: %w", adapter.AdapterName, controlstream.ErrAdapterNotFound)
		}
		return fmt.Errorf("adapter %q lookup failed: %w", adapter.AdapterName, err) // fail closed
	}
	for _, c := range fa.Spec.ServesRobotClasses {
		if c == robotClass {
			return nil
		}
	}
	if len(fa.Spec.ServesRobotClasses) == 0 {
		return fmt.Errorf("adapter %q does not serve robot class %q", adapter.AdapterName, robotClass)
	}
	return fmt.Errorf("adapter %q does not serve robot class %q: %w",
		adapter.AdapterName, robotClass, controlstream.ErrClassNotServed)
}
