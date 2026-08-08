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
// the Fleet Adapter ControlStream/SafetyStream (RFC-0001 §9.5.1.2, §9.2.7). It
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
// RobotClass is one the adapter declares it serves (§9.2.7). Anything else — an
// unverified identity, a missing robot, a different owner, an unserved class, or a
// lookup error — is denied.
func (a *Authorizer) AuthorizeRobot(ctx context.Context, adapter controlstream.TLSIdentity, robotID string) error {
	if !adapter.Verified {
		return fmt.Errorf("unauthenticated adapter: no verified mTLS identity")
	}
	if robotID == "" {
		return fmt.Errorf("empty robot_id")
	}

	robot := &fleetv1.Robot{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: robotID, Namespace: adapter.Namespace}, robot)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("robot %q not found in namespace %q", robotID, adapter.Namespace)
		}
		return fmt.Errorf("robot %q lookup failed: %w", robotID, err) // fail closed on transient errors
	}
	if robot.Spec.Adapter.Name != adapter.AdapterName {
		return fmt.Errorf("robot %q is bound to adapter %q, not %q", robotID, robot.Spec.Adapter.Name, adapter.AdapterName)
	}
	return a.authorizeClass(ctx, adapter, robot.Spec.RobotClass)
}

// AuthorizeAnnounce permits a first-time Discover unless the robot_id is already
// bound to a different adapter in the namespace. An unverified identity is denied.
func (a *Authorizer) AuthorizeAnnounce(ctx context.Context, adapter controlstream.TLSIdentity, robotID string) error {
	if !adapter.Verified {
		return fmt.Errorf("unauthenticated adapter: no verified mTLS identity")
	}
	if robotID == "" {
		return fmt.Errorf("empty robot_id")
	}

	robot := &fleetv1.Robot{}
	err := a.Client.Get(ctx, types.NamespacedName{Name: robotID, Namespace: adapter.Namespace}, robot)
	switch {
	case apierrors.IsNotFound(err):
		return nil // not yet admitted — a first announce is permitted
	case err != nil:
		return fmt.Errorf("robot %q lookup failed: %w", robotID, err) // fail closed
	case robot.Spec.Adapter.Name != adapter.AdapterName:
		return fmt.Errorf("robot %q is already bound to adapter %q", robotID, robot.Spec.Adapter.Name)
	default:
		return nil
	}
}

// authorizeClass checks that the adapter declares it serves robotClass.
func (a *Authorizer) authorizeClass(ctx context.Context, adapter controlstream.TLSIdentity, robotClass string) error {
	fa := &fleetv1.FleetAdapter{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: adapter.AdapterName, Namespace: adapter.Namespace}, fa); err != nil {
		return fmt.Errorf("adapter %q lookup failed: %w", adapter.AdapterName, err) // fail closed
	}
	for _, c := range fa.Spec.ServesRobotClasses {
		if c == robotClass {
			return nil
		}
	}
	return fmt.Errorf("adapter %q does not serve robot class %q", adapter.AdapterName, robotClass)
}
