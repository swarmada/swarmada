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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/swarmada/swarmada/internal/cli"
)

func newGetCommand(o *options) *cobra.Command {
	var discovered bool
	cmd := &cobra.Command{
		Use:   "get <resource> [name...]",
		Short: "Display one or many Swarmada resources",
		Long: `Display Swarmada resources in a kubectl-familiar table.

Columns mirror each CRD's own print columns; -o wide adds the CRD's
lower-priority columns, and -o yaml|json prints the full objects. Resources may
be named by plural, singular, kind, or short name (e.g. robots, robot, rob).`,
		Example: `  swarmctl get robot
  swarmctl get rob sim-robot-004 -o wide
  swarmctl get fleetaction -o yaml
  swarmctl get robot --discovered -n warehouse-a`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runGet(cmd.Context(), args[0], args[1:], discovered)
		},
	}
	cmd.Flags().BoolVar(&discovered, "discovered", false, "With `get robot`, list pre-admission DiscoveredRobots instead")
	cmd.AddCommand(newGetAuditCommand(o))
	return cmd
}

// discoveredTarget applies the --discovered filter: it is valid only on
// `get robot` with no name (a named resource resolves on its own), and swaps the
// read target to DiscoveredRobot.
func discoveredTarget(def *resourceDef, names []string) (*resourceDef, error) {
	if def.singular != "robot" {
		return nil, fmt.Errorf("--discovered applies only to `get robot`")
	}
	if len(names) > 0 {
		return nil, fmt.Errorf("--discovered cannot be combined with a named robot; a named resource resolves on its own")
	}
	return resolveResource("discoveredrobots")
}

func (o *options) runGet(ctx context.Context, resource string, names []string, discovered bool) error {
	def, err := resolveResource(resource)
	if err != nil {
		return err
	}
	if discovered {
		def, err = discoveredTarget(def, names)
		if err != nil {
			return err
		}
	}
	c, err := o.factory.Client()
	if err != nil {
		return err
	}
	ns, err := o.factory.Namespace()
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return o.getList(ctx, c, def, ns)
	}
	return o.getNamed(ctx, c, def, ns, names)
}

// getList lists every object of the kind in the namespace.
func (o *options) getList(ctx context.Context, c client.Client, def *resourceDef, ns string) error {
	list := def.newList()
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("listing %s: %w", def.plural, err)
	}
	objs := def.items(list)
	for _, obj := range objs {
		obj.GetObjectKind().SetGroupVersionKind(def.gvk)
	}

	if o.outputFmt.IsTable() {
		return o.renderTable(def, objs, ns)
	}
	// A list verb always marshals a List, even for a single item (kubectl parity).
	list.GetObjectKind().SetGroupVersionKind(def.gvk.GroupVersion().WithKind(def.kind + "List"))
	return cli.PrintMarshaled(o.streams.Out, o.outputFmt, list)
}

// getNamed fetches the explicitly named objects.
func (o *options) getNamed(ctx context.Context, c client.Client, def *resourceDef, ns string, names []string) error {
	objs := make([]client.Object, 0, len(names))
	for _, name := range names {
		obj := def.newObject()
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			return fmt.Errorf("getting %s/%s: %w", def.singular, name, err)
		}
		obj.GetObjectKind().SetGroupVersionKind(def.gvk)
		objs = append(objs, obj)
	}

	if o.outputFmt.IsTable() {
		return o.renderTable(def, objs, ns)
	}
	if len(objs) == 1 {
		return cli.PrintMarshaled(o.streams.Out, o.outputFmt, objs[0])
	}
	return cli.PrintMarshaled(o.streams.Out, o.outputFmt, objectList(objs))
}

// renderTable prints the colorized, aligned table for objs, or the kubectl
// "No resources found" notice (to stderr) when the set is empty.
func (o *options) renderTable(def *resourceDef, objs []client.Object, ns string) error {
	if len(objs) == 0 {
		_, _ = fmt.Fprintf(o.streams.Err, "No resources found in %s namespace.\n", ns)
		return nil
	}
	wide := o.outputFmt.IsWide()
	tbl := cli.NewTable(o.colorStdout, def.headers(wide)...)
	for _, obj := range objs {
		tbl.AddRow(def.row(obj, wide)...)
	}
	return tbl.Render(o.streams.Out)
}

// objectList wraps fetched objects in a v1 List for machine-readable output of a
// multi-name get, matching what `kubectl get a b -o yaml` produces.
func objectList(objs []client.Object) *metav1.List {
	l := &metav1.List{TypeMeta: metav1.TypeMeta{Kind: "List", APIVersion: "v1"}}
	for _, obj := range objs {
		l.Items = append(l.Items, runtime.RawExtension{Object: obj})
	}
	return l
}
