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
		Use:   "cancel [action|task] <name> --reason <text>",
		Short: "Cancel a FleetAction, or a FleetTask and the actions it is running",
		Long: `Cancel stops work that is already under way. Name the kind explicitly when it
matters: the two kinds are cancelled by different mechanisms. A bare name is still
a FleetAction, the form RFC-0001 documents.

  cancel action <fleetaction>
      Gated by the fleetactions/cancel custom verb (SelfSubjectAccessReview), then
      records the swarmada.io/cancel-requested annotation the FleetAction controller
      watches. Cancellation is not immediate: the controller finalizes it safely,
      holding a bound action until its adapter acknowledges or its assignment lease
      provably expires, so a robot mid-motion is never double-executed.

  cancel task <fleettask>
      Writes spec.desiredState: Cancelled on the FleetTask. The composite controller
      is authoritative for its children and fans that intent out to every non-terminal
      member action, so this cancels the actions the task is currently running as well
      as the task itself. It is a declarative write, not a custom verb: ordinary RBAC
      update permission on fleettasks already confers it, and re-writing the same value
      is idempotent. Once no member action is non-terminal the task reports phase
      Cancelled — unless failurePolicy is Compensate, which still rolls back the members
      that had already succeeded and ends at Compensated.

The spelling "cancel task <name>" previously cancelled a FleetAction. It now does what
it says — which also makes the FleetTask delete guard's advice ("cancel it first")
followable, where before it named a command that failed with not-found.`,
		Example: `  swarmctl cancel pick-run-4471 --reason "aisle blocked" --yes
  swarmctl cancel action pick-run-4471 --reason "aisle blocked" --yes
  swarmctl cancel task restock-aisle-7 --reason "shift ended"`,
		// One argument is a FleetAction name, which is the form RFC-0001 and
		// docs/operations.md document (`swarmctl cancel <fleetaction>`); keeping it
		// means this change adds spellings without invalidating published text.
		// Two arguments name the kind explicitly.
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return o.runActionCancel(cmd.Context(), args[0], reason, yes)
			}
			kind, name := args[0], args[1]
			def, err := resolveResource(kind)
			if err != nil {
				return fmt.Errorf("cancel takes a kind and a name: %w", err)
			}
			switch def.kind {
			case "FleetAction":
				return o.runActionCancel(cmd.Context(), name, reason, yes)
			case "FleetTask":
				return o.runTaskCancel(cmd.Context(), name, reason, yes)
			default:
				return fmt.Errorf("cannot cancel a %s; cancel operates on FleetAction or FleetTask", def.kind)
			}
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why it is being cancelled (recorded on the object)")
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

// actionCancel is the testable core of `swarmctl cancel action`: SSAR-gate, confirm,
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

func (o *options) runTaskCancel(ctx context.Context, name, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.taskCancel(ctx, c, cs, ns, name, reason, yes)
}

// taskCancel is the testable core of `swarmctl cancel task`. It writes
// spec.desiredState: Cancelled and stops there.
//
// It deliberately does NOT touch the member actions. The composite controller is
// authoritative for its children and fans desiredState out to every non-terminal
// member; a CLI that also wrote the children would be a second writer racing the
// controller for the same fields, which is the ownership violation the composite
// model exists to prevent. One write cancels the task AND the actions it is
// running, because that is what the controller does with it.
//
// There is no fleettasks/cancel custom verb, and adding one would confer nothing:
// this is an ordinary update to spec, so RBAC update permission on fleettasks
// already carries the authority. A custom verb is warranted where an action is
// irreversible, safety-relevant, or grants authority the underlying verb does not
// already imply. This is none of those at the API layer — the safety-relevant part
// is the per-action stop, which the FleetAction path gates with its own verb when
// the controller fans the cancellation down.
func (o *options) taskCancel(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool) error {
	if err := cli.RequireVerb(ctx, cs, "update", "fleettasks", ns, name); err != nil {
		return err
	}

	task := &fleetv1.FleetTask{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, task); err != nil {
		return fmt.Errorf("getting fleettask/%s: %w", name, err)
	}

	if task.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		_, _ = fmt.Fprintf(o.streams.Out, "fleettask.swarmada.io/%s is already cancelling.\n", name)
		return nil
	}

	// Naming the member count makes the blast radius visible before the operator
	// agrees to it: cancelling a task cancels everything it is running.
	prompt := fmt.Sprintf("Cancel fleettask %q in %q, and its %d member action(s)?",
		name, ns, len(task.Spec.Actions))
	ok, err := cli.Confirm(o.streams, prompt, yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	base := task.DeepCopy()
	task.Spec.DesiredState = fleetv1.DesiredStateCancelled
	if err := c.Patch(ctx, task, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("setting desiredState on fleettask/%s: %w", name, err)
	}

	// The reason has nowhere to live on a FleetTask: unlike FleetAction there is no
	// cancel-requested annotation, because the mechanism is a spec write rather than
	// a request the controller finalizes. Recording it as an annotation would invent
	// an API field the specification does not define, so say so rather than dropping
	// it silently.
	if reason != "" {
		_, _ = fmt.Fprintf(o.streams.Err,
			"note: --reason is not recorded on a FleetTask (no such field in RFC-0001); "+
				"it is preserved on each member action the controller cancels.\n")
	}

	_, _ = fmt.Fprintf(o.streams.Out,
		"fleettask.swarmada.io/%s desiredState set to Cancelled; %d member action(s) will be cancelled by the controller.\n",
		name, len(task.Spec.Actions))
	return nil
}
