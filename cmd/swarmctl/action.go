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

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func newCancelCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "cancel task <fleetaction> --reason <text>",
		Short: "Request cancellation of a FleetAction (custom verb: cancel)",
		Long: `Cancel requests that a FleetAction be cancelled. It is gated by the
fleetactions/cancel custom verb (SelfSubjectAccessReview) and then records the
request as the swarmada.io/cancel-requested annotation the FleetAction controller
watches. Cancellation is not immediate: the controller finalizes it safely,
holding a bound action until its adapter acknowledges or its assignment lease
provably expires, so a robot mid-motion is never double-executed.`,
		Example: `  swarmctl cancel task pick-run-4471 --reason "aisle blocked" --yes`,
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Optional leading "task" kind token (`cancel task <name>`); a bare
			// `cancel <name>` is also accepted.
			if len(args) == 2 {
				if args[0] != "task" {
					return fmt.Errorf("cancel takes a FleetAction name, optionally prefixed with \"task\"")
				}
				args = args[1:]
			}
			return o.runActionCancel(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the task is being cancelled (recorded on the task)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

func (o *options) runActionCancel(ctx context.Context, name, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.actionCancel(ctx, c, cs, ns, name, reason, yes)
}

// actionCancel is the testable core of `swarmctl cancel task`: SSAR-gate, confirm,
// then record the cancel-requested annotation the controller finalizes on.
func (o *options) actionCancel(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool) error {
	// Custom-verb gate first, fail closed.
	if err := cli.RequireVerb(ctx, cs, "cancel", "fleetactions", ns, name); err != nil {
		return err
	}

	// Cancellation is destructive to in-flight work: confirm unless --yes.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Cancel fleetaction %q in %q?", name, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	action := &fleetv1.FleetAction{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, action); err != nil {
		return fmt.Errorf("getting fleetaction/%s: %w", name, err)
	}

	// The annotation value is the reason; the controller reads it verbatim into
	// status.message on finalize. Default to "true" so the request still fires
	// when no reason is given (an empty value would read as "not requested").
	val := reason
	if val == "" {
		val = "true"
	}
	if err := patchAnnotation(ctx, c, action, annCancelRequested, val); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(o.streams.Out, "fleetaction.swarmada.io/%s cancellation requested.\n", name)
	return nil
}
