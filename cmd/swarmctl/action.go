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
      that had already succeeded and ends at Compensated. A task that has already
      ended (Succeeded, Failed, Cancelled or Compensated) is not reconciled again, so
      the write cancels none of its members; cancel a member that is still running
      with cancel action.

The spelling "cancel task <name>" previously cancelled a FleetAction. It now does what
it says — which also makes the FleetTask delete guard's advice ("cancel it first")
followable, where before it named a command that failed with not-found.

A bare name that turns out to belong to the other kind is named as such: cancelling a
FleetTask by bare name reports that it is a FleetTask and prints the command that works,
rather than "fleetaction/<name> not found".`,
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
			resolved, err := resolveCancelKind(kind)
			if err != nil {
				return err
			}
			switch resolved {
			case "FleetAction":
				return o.runActionCancel(cmd.Context(), name, reason, yes)
			default:
				return o.runTaskCancel(cmd.Context(), name, reason, yes)
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

// actionCancel is the testable core of `swarmctl cancel action`: fetch, gate,
// confirm, then record the cancel-requested annotation the controller finalizes on.
// The order is the package rule documented in lifecycle.go.
func (o *options) actionCancel(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool) error {
	// 1. Fetch. A not-found name is reported as not-found — and, when the name is
	//    a FleetTask, as the wrong kind with the command that does work — rather
	//    than as a permission problem or a prompt for an object that is not there.
	action := &fleetv1.FleetAction{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, action); err != nil {
		if apierrors.IsNotFound(err) {
			if taskExists(ctx, c, ns, name) {
				return fmt.Errorf("%s/%s is a FleetTask, not a FleetAction; "+
					"use `swarmctl cancel task %s` to cancel it and the actions it is running",
					ns, name, name)
			}
		}
		return fmt.Errorf("getting fleetaction/%s: %w", name, err)
	}

	// 2. Authorise, fail closed, before the prompt: the custom cancel verb, and patch,
	//    which the API server checks on the annotation write below.
	for _, verb := range []string{"patch", "cancel"} {
		if err := cli.RequireVerb(ctx, cs, verb, "fleetactions", ns, name); err != nil {
			return err
		}
	}

	// 3. Confirm. Cancellation is destructive to in-flight work, and the object
	//    named in the prompt has now been verified to exist.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Cancel fleetaction %q in %q?", name, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
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
// Cancelling needs two verbs on fleettasks, and the pre-check asks for exactly
// those two: `patch`, because the write below is a merge patch and the API server
// authorizes it as one, and `cancel`, because with webhooks enabled the FleetTask
// validating webhook refuses a move of spec.desiredState to Cancelled without it
// (ValidateUpdate, internal/webhook/fleettask_webhook.go). `update` is neither, so
// it is not asked for. The fan-out to members is not verb-gated: the controller
// patches each non-terminal member's spec.desiredState under its own identity, and
// the FleetAction webhook gates only the swarmada.io/cancel-requested annotation,
// which the fan-out does not write.
func (o *options) taskCancel(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool) error {
	// Fetch, authorise, prompt — the package rule in lifecycle.go. The symmetric
	// hint to the FleetAction path: a FleetAction name given to `cancel task` is
	// named as such rather than reported as a missing FleetTask.
	task := &fleetv1.FleetTask{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			if actionExists(ctx, c, ns, name) {
				return fmt.Errorf("%s/%s is a FleetAction, not a FleetTask; "+
					"use `swarmctl cancel action %s` to cancel it", ns, name, name)
			}
		}
		return fmt.Errorf("getting fleettask/%s: %w", name, err)
	}

	for _, verb := range []string{"patch", "cancel"} {
		if err := cli.RequireVerb(ctx, cs, verb, "fleettasks", ns, name); err != nil {
			return err
		}
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
			"note: --reason is not recorded on a FleetTask, which has no field for it, and the controller's "+
				"cancellation of each member action carries no reason\n")
	}

	// A task that has ended is not reconciled again (the controller returns on a terminal
	// phase), so saying its members will be cancelled would be false.
	switch task.Status.Phase {
	case fleetv1.FleetTaskPhaseSucceeded, fleetv1.FleetTaskPhaseFailed,
		fleetv1.FleetTaskPhaseCancelled, fleetv1.FleetTaskPhaseCompensated:
		_, _ = fmt.Fprintf(o.streams.Out,
			"fleettask.swarmada.io/%s desiredState set to Cancelled; the task has already ended (%s), so the "+
				"controller cancels none of its member actions.\nto stop a member action that is still running: "+
				"swarmctl cancel action <name>\n",
			name, task.Status.Phase)
		return nil
	}
	_, _ = fmt.Fprintf(o.streams.Out,
		"fleettask.swarmada.io/%s desiredState set to Cancelled; %d member action(s) will be cancelled by the controller.\n",
		name, len(task.Spec.Actions))
	return nil
}

// taskExists / actionExists are the wrong-kind probes. They run only on the
// not-found path of the other kind, so the common case costs no extra read, and
// they answer the question the operator actually asked: "is the thing I named of
// the other kind?" A read failure (including Forbidden) is reported as "no",
// because the purpose is to improve an error message, never to gate anything.
func taskExists(ctx context.Context, c client.Client, ns, name string) bool {
	return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &fleetv1.FleetTask{}) == nil
}

func actionExists(ctx context.Context, c client.Client, ns, name string) bool {
	return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &fleetv1.FleetAction{}) == nil
}

// cancelKinds is the kind vocabulary `cancel` accepts, over and above the
// registry spellings. `action` and `task` are the tokens this command's own Use,
// Long and Example print, and the tokens docs/operations.md prints — and neither
// resolved, so every documented two-token form failed with `unknown resource`.
//
// They are deliberately LOCAL to cancel rather than registered as global aliases.
// Registering them would make `get task` and `describe task` resolve too, which
// reintroduces exactly the ambiguity this command exists to remove: "task" alone
// does not distinguish the composite from the atomic unit, and the Conventions
// section of docs/operations.md states that it does not resolve as a resource.
// Inside `cancel` the ambiguity is gone, because the grammar is `[action|task]`
// and each token names one kind.
var cancelKinds = map[string]string{
	"action": "FleetAction",
	"task":   "FleetTask",
}

// resolveCancelKind maps a kind token to the kind cancel will operate on,
// accepting both the command-local vocabulary and any registered spelling.
func resolveCancelKind(token string) (string, error) {
	if k, ok := cancelKinds[strings.ToLower(token)]; ok {
		return k, nil
	}
	def, err := resolveResource(token)
	if err != nil {
		return "", fmt.Errorf("cancel takes a kind and a name: %q is neither: "+
			"use action|fleetaction|fact or task|fleettask|ft", token)
	}
	switch def.kind {
	case "FleetAction", "FleetTask":
		return def.kind, nil
	default:
		return "", fmt.Errorf("cannot cancel a %s; cancel operates on FleetAction or FleetTask", def.kind)
	}
}
