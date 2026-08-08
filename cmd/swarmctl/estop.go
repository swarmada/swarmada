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
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func newEstopCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "estop",
		Short: "Trigger an emergency stop on a FleetZone",
		Long: `Trigger an emergency stop on a FleetZone (RFC-0001 §9.6.2.5): a confirmed
stop of every robot in the zone (and, per estopPolicy, its descendants) via the
estop-trigger custom verb, SelfSubjectAccessReview-gated, so RBAC and the
tamper-evident safety audit log apply. Clearing an estop is the separate,
admin-only top-level command "swarmctl estop-clear".`,
	}
	cmd.AddCommand(newEstopTriggerCommand(o))
	return cmd
}

func newEstopTriggerCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:     "trigger <fleetzone> --reason <text>",
		Short:   "Trigger an emergency stop on a FleetZone (custom verb: estop-trigger)",
		Example: `  swarmctl estop trigger zone-aisle-b3 --reason "person detected" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runEstopTrigger(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the estop is being triggered (recorded in the safety audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

func newEstopClearCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "estop-clear <fleetzone> --reason <text>",
		Short: "Clear an emergency stop on a FleetZone (custom verb: estop-clear, admin-only)",
		Long: `Clear an active zone emergency stop and resume its robots. Only the
swarmada:admin role holds the estop-clear verb (RFC-0001 §9.5.3.2): clearing an
estop is a safety decision, so automation and non-admin operators are denied by
the SelfSubjectAccessReview gate. Actions stay operator-gated after a clear.

--reason is required and must not be empty or whitespace; it is written to the
tamper-evident safety audit log (ESTOP_CLEARED).`,
		Example: `  swarmctl estop-clear zone-aisle-b3 --reason "aisle clear, inspected" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runEstopClear(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the estop is being cleared (required, recorded in the safety audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (o *options) runEstopTrigger(ctx context.Context, zone, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.estopTrigger(ctx, c, cs, ns, zone, reason, yes)
}

// estopTrigger is the testable core of `swarmctl estop trigger`.
func (o *options) estopTrigger(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, zone, reason string, yes bool) error {
	if err := cli.RequireVerb(ctx, cs, "estop-trigger", "fleetzones", ns, zone); err != nil {
		return err
	}
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Trigger an EMERGENCY STOP on fleetzone %q in %q? This stops every robot in the zone.", zone, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	fz := &fleetv1.FleetZone{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}
	// The controller re-fires on a NEW annotation value and is idempotent on the
	// same one; the reason is the value (default "true" when none is given).
	val := reason
	if val == "" {
		val = "true"
	}
	if err := patchAnnotation(ctx, c, fz, annEstopTriggered, val); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(o.streams.Out, "fleetzone.swarmada.io/%s emergency stop triggered.\n", zone)
	return nil
}

func (o *options) runEstopClear(ctx context.Context, zone, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.estopClear(ctx, c, cs, ns, zone, reason, yes)
}

// estopClear is the testable core of `swarmctl estop-clear`. --reason is required
// and must be non-empty (validated here so callers of the core enforce it too);
// it is recorded in the safety audit log as ESTOP_CLEARED.
func (o *options) estopClear(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, zone, reason string, yes bool) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required and must not be empty")
	}
	if err := cli.RequireVerb(ctx, cs, "estop-clear", "fleetzones", ns, zone); err != nil {
		return err
	}
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Clear the emergency stop on fleetzone %q in %q and resume its robots?", zone, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	fz := &fleetv1.FleetZone{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}
	// Clearing = removing the trigger annotation; the controller sees the trigger
	// gone (with its processed marker still set) and resumes the zone's robots.
	if err := removeAnnotation(ctx, c, fz, annEstopTriggered); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(o.streams.Out, "fleetzone.swarmada.io/%s emergency stop cleared (reason: %s).\n", zone, reason)
	return nil
}

// lifecycleClients builds the controller-runtime client, the clientset (for the
// SSAR gate), and resolves the namespace — the trio every lifecycle verb needs.
func (o *options) lifecycleClients() (client.Client, kubernetes.Interface, string, error) {
	c, err := o.factory.Client()
	if err != nil {
		return nil, nil, "", err
	}
	cs, err := o.factory.Clientset()
	if err != nil {
		return nil, nil, "", err
	}
	ns, err := o.factory.Namespace()
	if err != nil {
		return nil, nil, "", err
	}
	return c, cs, ns, nil
}
