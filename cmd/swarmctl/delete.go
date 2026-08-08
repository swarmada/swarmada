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

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func newDeleteCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete Swarmada resources",
	}
	cmd.AddCommand(newDeleteRobotCommand(o))
	return cmd
}

func newDeleteRobotCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "robot <name> [--reason <text>]",
		Short: "Delete an admitted Robot, or reject a discovered one (custom verb: reject|delete)",
		Long: `Delete resolves the target by what the name names. A DiscoveredRobot is
rejected — the reject custom verb deletes it and records --reason as a namespace
Event (RFC-0001 §9.1.2.7). An admitted Robot is a plain delete, gated by the
standard delete RBAC verb. RBAC still distinguishes the two, so "who may reject a
discovered robot" and "who may delete a live robot" stay separate.`,
		Example: `  swarmctl delete robot dr-acme-a3f9 --reason "stale test entry"
  swarmctl delete robot amr-acme-042`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runDeleteRobot(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the robot is being removed (recorded as an Event when rejecting a discovered robot)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// runDeleteRobot dispatches on what <name> resolves to: a DiscoveredRobot takes
// the reject path (reason recorded as an Event), an admitted Robot a plain
// delete. A missing --reason is only advisory, so it warns rather than fails.
func (o *options) runDeleteRobot(ctx context.Context, name, reason string, yes bool) error {
	if strings.TrimSpace(reason) == "" {
		_, _ = fmt.Fprintln(o.streams.Err, "warning: no --reason given; a rejection reason is recorded in the namespace Event trail.")
	}
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}

	// Prefer the DiscoveredRobot (pre-admission) target if one exists; otherwise
	// treat the name as an admitted Robot.
	dr := &fleetv1.DiscoveredRobot{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, dr); {
	case err == nil:
		return o.reject(ctx, c, cs, ns, name, reason, yes)
	case apierrors.IsNotFound(err):
		// fall through to the admitted-robot path
	default:
		return fmt.Errorf("getting discoveredrobot/%s: %w", name, err)
	}
	return o.deleteAdmittedRobot(ctx, c, ns, name, yes)
}

// deleteAdmittedRobot is the testable core of deleting a live Robot: confirm,
// then a plain delete. RBAC on the standard delete verb is enforced server-side
// by the API on the Delete call itself.
func (o *options) deleteAdmittedRobot(ctx context.Context, c client.Client, ns, name string, yes bool) error {
	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
		return fmt.Errorf("getting robot/%s: %w", name, err)
	}
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Delete robot %q in %q?", name, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}
	if err := c.Delete(ctx, robot); err != nil {
		return fmt.Errorf("deleting robot/%s: %w", name, err)
	}
	_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s deleted.\n", name)
	return nil
}
