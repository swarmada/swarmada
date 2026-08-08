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

// resetAnnotation mirrors internal/controller's constant. The CLI writes the request; the
// controller owns the status change — the same split estop and admit use, so `status` is never
// edited from a client.
const policyResetAnnotation = "swarmada.io/policy-reset"

func newModelPolicyCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "modelpolicy",
		Short: "Operate on ModelPolicy resources",
	}
	cmd.AddCommand(newModelPolicyResetCommand(o))
	return cmd
}

func newModelPolicyResetCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset <modelpolicy> --reason <text>",
		Short: "Clear a FailedRepeatedly suspension on a ModelPolicy (custom verb: policy-reset)",
		Long: `Clear a ModelPolicy's FailedRepeatedly suspension and resume evaluation
(RFC-0001 §9.1.9.4).

After spec.consecutiveRejectionLimit consecutive quality-gate rejections the
controller suspends the policy: polling stops and further triggers are dropped.
This is the documented operator recovery path — without it a suspension can only
be cleared by hand-editing status, which bypasses both RBAC and the audit trail.

It is an OVERRIDE of an automated decision, not an approval of a model: the
quality gate's own Deploy/Reject verdicts are untouched, and the next trigger is
evaluated on its merits. If the underlying quality problem persists, the policy
will suspend again.

Gated by a SelfSubjectAccessReview on the policy-reset custom verb, so the same
RBAC that governs admit/reject governs this (§9.5.3).`,
		Example: `  swarmctl modelpolicy reset nav-model-gate --reason "metrics regression fixed in v4.2" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runModelPolicyReset(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the suspension is being cleared (recorded on the resource)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func (o *options) runModelPolicyReset(ctx context.Context, name, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.modelPolicyReset(ctx, c, cs, ns, name, reason, yes)
}

// modelPolicyReset is the testable core of `swarmctl modelpolicy reset`.
func (o *options) modelPolicyReset(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool) error {
	// Fail closed: no verb, no reset. Checked BEFORE reading the resource so a denied caller
	// learns nothing about whether the policy exists or is suspended.
	if err := cli.RequireVerb(ctx, cs, "policy-reset", "modelpolicies", ns, name); err != nil {
		return err
	}

	mp := &fleetv1.ModelPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, mp); err != nil {
		return fmt.Errorf("getting modelpolicy/%s: %w", name, err)
	}

	ok, err := cli.Confirm(o.streams,
		fmt.Sprintf("Clear the suspension on modelpolicy %q in %q and resume evaluation?", name, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	// A NEW annotation value is what makes the controller act; the reason is that value, so a
	// second reset after a second suspension re-fires rather than being deduplicated away.
	val := reason
	if val == "" {
		val = "operator reset"
	}
	base := mp.DeepCopy()
	if mp.Annotations == nil {
		mp.Annotations = map[string]string{}
	}
	mp.Annotations[policyResetAnnotation] = val
	if err := c.Patch(ctx, mp, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("requesting reset of modelpolicy/%s: %w", name, err)
	}

	_, _ = fmt.Fprintf(o.streams.Out, "modelpolicy/%s reset requested; evaluation resumes once the controller reconciles\n", name)
	return nil
}
