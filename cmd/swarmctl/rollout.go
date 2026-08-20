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

// rolloutResumeAnnotation mirrors internal/controller's constant. The CLI writes the request;
// the controller owns the status change — the same split estop, admit and policy-reset use, so
// `status` is never edited from a client.
const rolloutResumeAnnotation = "swarmada.io/rollout-resume"

// rolloutResumeVerb is the custom verb the SelfSubjectAccessReview is run against (§9.5.3).
const rolloutResumeVerb = "rollout-resume"

func newRolloutCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollout",
		Short: "Operate on FirmwareRollout and ModelRollout resources",
	}
	cmd.AddCommand(newRolloutResumeCommand(o))
	return cmd
}

func newRolloutResumeCommand(o *options) *cobra.Command {
	var reason string
	var kind string
	var yes bool
	cmd := &cobra.Command{
		Use:   "resume <rollout> --kind firmware|model --reason <text>",
		Short: "Resume a Paused rollout, excluding the robots that failed (custom verb: rollout-resume)",
		Long: `Resume a rollout halted by strategy.rollingUpdate.pauseOnError.

pauseOnError defaults to true, so a single failed robot stops the rollout entering
any further robot into the batch. This is the operator path past that halt.

IT EXCLUDES, IT DOES NOT RETRY. The robots that failed are recorded in
status.excludedRobots and this rollout will never attempt them again — resuming
would otherwise re-dispatch the artifact that just failed and re-pause on the same
robots. To retry them, fix the artifact and create a FRESH rollout (ADR-0041).

Excluding them is also what lets the rollout finish: they count as settled, so the
rollout can reach a terminal phase, and only a terminal record may be deleted.

Gated by a SelfSubjectAccessReview on the rollout-resume custom verb (§9.5.3), and
sealed into the safety audit log as ROLLOUT_RESUMED.`,
		Example: `  swarmctl rollout resume nav-model-v4 --kind model --reason "amr-7 has a failed SSD; excluding it" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runRolloutResume(cmd.Context(), args[0], kind, reason, yes)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "rollout kind: firmware or model (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the rollout is being resumed (recorded on the resource and in the audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func (o *options) runRolloutResume(ctx context.Context, name, kind, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.rolloutResume(ctx, c, cs, ns, name, kind, reason, yes)
}

// rolloutResumeTarget resolves --kind to the object to patch and the resource name the
// SelfSubjectAccessReview is run against.
func rolloutResumeTarget(kind string) (client.Object, string, error) {
	switch kind {
	case "firmware":
		return &fleetv1.FirmwareRollout{}, "firmwarerollouts", nil
	case "model":
		return &fleetv1.ModelRollout{}, "modelrollouts", nil
	case "":
		return nil, "", fmt.Errorf("--kind is required (firmware or model)")
	default:
		return nil, "", fmt.Errorf("unknown rollout kind %q: want firmware or model", kind)
	}
}

// rolloutResume is the testable core of `swarmctl rollout resume`.
func (o *options) rolloutResume(ctx context.Context, c client.Client, cs kubernetes.Interface,
	ns, name, kind, reason string, yes bool) error {
	obj, resource, err := rolloutResumeTarget(kind)
	if err != nil {
		return err
	}

	// Fail closed: no verb, no resume. Checked BEFORE reading the resource so a denied caller
	// learns nothing about whether the rollout exists or is paused.
	if err := cli.RequireVerb(ctx, cs, rolloutResumeVerb, resource, ns, name); err != nil {
		return err
	}

	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		return fmt.Errorf("getting %s/%s: %w", resource, name, err)
	}

	ok, err := cli.Confirm(o.streams, fmt.Sprintf(
		"Resume %s %q in %q? The robots that failed will be EXCLUDED from this rollout and never retried by it.",
		resource, name, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	// A NEW annotation value is what makes the controller act; the reason is that value, so a
	// second resume after a second pause re-fires rather than being deduplicated away.
	val := reason
	if val == "" {
		val = "operator resume"
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[rolloutResumeAnnotation] = val
	obj.SetAnnotations(ann)
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("requesting resume of %s/%s: %w", resource, name, err)
	}

	_, _ = fmt.Fprintf(o.streams.Out,
		"%s/%s resume requested; the rollout advances once the controller reconciles\n", resource, name)
	return nil
}
