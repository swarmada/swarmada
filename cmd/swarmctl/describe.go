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
	"io"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// describeValueCol is the column at which describe values begin, so the block
// scans vertically (kubectl-style).
const describeValueCol = 24

func newDescribeCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <resource> <name> [name...]",
		Short: "Show the details of a Swarmada resource",
		Long: `Show a resource's metadata, spec/status fields, conditions, and events in a
kubectl describe-style layout. Status-signal values (phase, battery, hardware
and condition health) are colorized; everything else is plain text.`,
		Example: `  swarmctl describe robot sim-robot-004
  swarmctl describe fleetaction pick-run-4471 -n warehouse-a`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runDescribe(cmd.Context(), args[0], args[1:])
		},
	}
}

func (o *options) runDescribe(ctx context.Context, resource string, names []string) error {
	def, err := resolveResource(resource)
	if err != nil {
		return err
	}
	c, err := o.factory.Client()
	if err != nil {
		return err
	}
	ns, err := o.factory.Namespace()
	if err != nil {
		return err
	}

	for i, name := range names {
		obj := def.newObject()
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			return fmt.Errorf("getting %s/%s: %w", def.singular, name, err)
		}
		if i > 0 {
			_, _ = fmt.Fprintln(o.streams.Out)
		}
		if r, ok := obj.(*fleetv1.Robot); ok {
			o.describeRobot(r)
			continue
		}
		o.describeGeneric(def, obj)
	}
	return nil
}

// describeGeneric renders the shared describe layout for any kind: metadata
// header, API version/kind, the kind's detail lines, conditions, and events.
func (o *options) describeGeneric(def *resourceDef, obj client.Object) {
	w := o.streams.Out
	cli.DescribeHeader(w, obj)
	_, _ = fmt.Fprintf(w, "API Version:  %s\n", fleetv1.GroupVersion.String())
	_, _ = fmt.Fprintf(w, "Kind:         %s\n", def.kind)
	if def.details != nil {
		cli.DescribeDetails(w, def.details(obj), describeValueCol)
	}
	if def.conditions != nil {
		cli.DescribeConditions(w, o.colorStdout, def.conditions(obj))
	}
	_, _ = fmt.Fprintln(w, "Events:  <none>")
}

// describeRobot renders the Robot describe view: the CLI style guide's
// "describe robot" layout and coloring, with
// every value sourced from real Robot CRD fields (the reference mock's "Fleet
// Adapter Endpoint" is not a Robot field, so the real spec.adapter reference is
// shown instead).
func (o *options) describeRobot(r *fleetv1.Robot) {
	w := o.streams.Out
	color := o.colorStdout
	cli.DescribeHeader(w, &r.ObjectMeta)
	_, _ = fmt.Fprintf(w, "API Version:  %s\n", fleetv1.GroupVersion.String())
	_, _ = fmt.Fprintln(w, "Kind:         Robot")
	_, _ = fmt.Fprintf(w, "Manufacturer: %s\n", r.Spec.Manufacturer)
	_, _ = fmt.Fprintf(w, "Model:        %s\n", r.Spec.Model)
	_, _ = fmt.Fprintf(w, "Zone:         %s\n", r.Spec.Zone)
	_, _ = fmt.Fprintf(w, "Robot Class:  %s\n", orNone(r.Spec.RobotClass))
	_, _ = fmt.Fprintf(w, "Adapter:      %s (v%s)\n", r.Spec.Adapter.Name, r.Spec.Adapter.Version)

	_, _ = fmt.Fprintln(w, "Status:")
	_, _ = fmt.Fprintf(w, "  Phase:                 %s\n", cli.RobotPhaseCell(string(r.Status.Phase)).Render(color))
	_, _ = fmt.Fprintf(w, "  Battery:               %s\n", robotBatteryLine(color, r))
	_, _ = fmt.Fprintf(w, "  Assigned Task:         %s\n", orNone(r.Status.AssignedAction))
	_, _ = fmt.Fprintf(w, "  Current Zone:          %s\n", orNone(r.Status.CurrentZone))
	_, _ = fmt.Fprintf(w, "  Estop State:           %s\n", orNone(string(r.Status.EstopState)))
	if r.Status.Connectivity != nil {
		_, _ = fmt.Fprintf(w, "  Last Seen:             %s\n", cli.AgePtr(r.Status.Connectivity.LastSeenAt))
	}
	_, _ = fmt.Fprintf(w, "  Observed Generation:   %d\n", r.Status.ObservedGeneration)

	if p := r.Status.Position; p != nil {
		// The position caveat is a direct RA-1 callout: coarse, display-only, so
		// nobody builds tooling against it as a live pose feed.
		_, _ = fmt.Fprintln(w, "  Position (coarse, display-only — query the telemetry TSDB for live pose):")
		_, _ = fmt.Fprintf(w, "    X: %.2f   Y: %.2f   Yaw: %.2f\n", p.X, p.Y, p.Yaw)
	}

	if len(r.Status.Hardware) > 0 {
		_, _ = fmt.Fprintln(w, "  Hardware:")
		hw := cli.NewTable(color, "NAME", "STATUS")
		for _, h := range r.Status.Hardware {
			hw.AddRow(cli.TextCell(h.Name), cli.HardwareStatusCell(string(h.Status)))
		}
		indent(w, hw, "    ")
	}

	cli.DescribeConditions(w, color, r.Status.Conditions)
	_, _ = fmt.Fprintln(w, "Events:  <none>")
}

// robotBatteryLine renders "45% [█████████░░░░░░░░░░░]" colored by level, or the
// em-dash placeholder when the coarse projection has never been written.
func robotBatteryLine(color bool, r *fleetv1.Robot) string {
	if r.Status.BatteryPercent == nil {
		return cli.Unknown
	}
	label, bar := cli.BatteryBar(color, int(*r.Status.BatteryPercent), r.Status.Phase == fleetv1.RobotPhaseCharging)
	return fmt.Sprintf("%s [%s]", label, bar)
}

// indent renders a table and re-emits each line with a prefix (describe nesting).
func indent(w io.Writer, tbl *cli.Table, prefix string) {
	cli.IndentTable(w, tbl, prefix)
}
