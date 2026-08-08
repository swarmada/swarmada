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
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func newRobotClassCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "robotclass",
		Short: "Manage RobotClasses",
	}
	cmd.AddCommand(newRobotClassRolloutCommand(o))
	return cmd
}

func newRobotClassRolloutCommand(o *options) *cobra.Command {
	var zone string
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollout <class>",
		Short: "Re-admit every Robot of a RobotClass to pick up a class change",
		Long: `Rollout re-admits every Robot referencing a RobotClass, one at a time, so a
class change reaches the whole fleet. This is the interim, scripted-loop path
RFC-0001 §9.1.1 prescribes until the RobotClassRollout resource (a future RFC)
provides batched, drain-aware rollouts; it does NOT itself drain in-progress
actions or honor maintenance windows, so sequence it during a quiet window.`,
		Example: `  swarmctl robotclass rollout acme-picker-v2 --yes
  swarmctl robotclass rollout acme-picker-v2 --zone zone-aisle-b3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.factory.Client()
			if err != nil {
				return err
			}
			ns, err := o.factory.Namespace()
			if err != nil {
				return err
			}
			return o.robotClassRollout(cmd.Context(), c, ns, args[0], zone, yes)
		},
	}
	cmd.Flags().StringVar(&zone, "zone", "", "Only re-admit robots in this zone")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// robotClassRollout re-admits every Robot whose spec.robotClass matches className
// (optionally filtered to a zone). It confirms once for the whole batch, then
// re-applies the class template to each robot, reporting a per-robot result.
func (o *options) robotClassRollout(ctx context.Context, c client.Client, ns, className, zone string, yes bool) error {
	class := &fleetv1.RobotClass{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: className}, class); err != nil {
		return fmt.Errorf("getting robotclass/%s: %w", className, err)
	}

	list := &fleetv1.RobotList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("listing robots: %w", err)
	}
	var targets []*fleetv1.Robot
	for i := range list.Items {
		r := &list.Items[i]
		if r.Spec.RobotClass != className {
			continue
		}
		if zone != "" && r.Spec.Zone != zone {
			continue
		}
		targets = append(targets, r)
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintf(o.streams.Err, "No robots reference robotclass %q%s.\n", className, zoneSuffix(zone))
		return nil
	}

	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Re-admit %d robot(s) of class %q%s? This changes their declared capabilities.", len(targets), className, zoneSuffix(zone)), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	var updated, failed int
	for _, r := range targets {
		base := r.DeepCopy()
		applyClassToSpec(&r.Spec, class, "")
		if err := c.Patch(ctx, r, client.MergeFrom(base)); err != nil {
			failed++
			_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s re-admit FAILED: %v\n", r.Name, err)
			continue
		}
		updated++
		_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s re-admitted\n", r.Name)
	}

	_, _ = fmt.Fprintf(o.streams.Out, "rollout of class %s: %d re-admitted, %d failed\n", className, updated, failed)
	if failed > 0 {
		return fmt.Errorf("%d robot(s) failed to re-admit", failed)
	}
	return nil
}

func zoneSuffix(zone string) string {
	if zone == "" {
		return ""
	}
	return " in zone " + zone
}
