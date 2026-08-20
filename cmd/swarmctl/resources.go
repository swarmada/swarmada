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
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// resourceDef describes one Swarmada CRD to the read verbs. Columns mirror the
// type's own +kubebuilder:printcolumn markers (api-principles.md
// § Discoverability): the CLI renders the API's discoverability contract, it
// does not redesign it. `wideOnly` columns carry the CRD's priority>=1 markers.
type resourceDef struct {
	singular string
	plural   string
	kind     string
	aliases  []string // shortName and any extra spellings
	gvk      schema.GroupVersionKind

	newList func() client.ObjectList
	// items returns the list's elements as addressable client.Objects.
	items func(client.ObjectList) []client.Object
	// newObject returns a zero object for a single get/describe.
	newObject func() client.Object

	// columns declares headers and cell extractors in CRD-declared order.
	columns []column
	// details returns the describe key/value lines for this kind's spec/status.
	details func(client.Object) []cli.KV
	// conditions returns status.conditions, or nil if the kind has none.
	conditions func(client.Object) []metav1.Condition
}

// column is one print column: a header, a cell extractor, and whether it is a
// wide-only (priority>=1) column.
type column struct {
	header string
	cell   func(client.Object) cli.Cell
	wide   bool
}

// headers returns the column headers for the requested width. NAME is always
// first (added by the get command), so this covers only the CRD's own columns.
func (r *resourceDef) headers(wide bool) []string {
	out := []string{"NAME"}
	for _, c := range r.columns {
		if c.wide && !wide {
			continue
		}
		out = append(out, c.header)
	}
	return out
}

// row renders one object's cells for the requested width.
func (r *resourceDef) row(obj client.Object, wide bool) []cli.Cell {
	out := []cli.Cell{cli.TextCell(obj.GetName())}
	for _, c := range r.columns {
		if c.wide && !wide {
			continue
		}
		out = append(out, c.cell(obj))
	}
	return out
}

// registry maps every alias (plural, singular, kind-lowercase, shortName) to its
// definition. Built once at init from resourceDefs.
var registry = map[string]*resourceDef{}

// resourceOrder preserves declaration order for help text / `get all`-style use.
var resourceOrder []*resourceDef

func registerResource(r *resourceDef) {
	resourceOrder = append(resourceOrder, r)
	for _, a := range append([]string{r.plural, r.singular, strings.ToLower(r.kind)}, r.aliases...) {
		registry[a] = r
	}
}

// resolveResource looks up a resource by any of its accepted spellings.
func resolveResource(name string) (*resourceDef, error) {
	if r, ok := registry[strings.ToLower(name)]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("unknown resource %q; run `swarmctl get` for the supported kinds", name)
}

// --- small cell/value helpers -------------------------------------------------

func text(s string) cli.Cell { return cli.TextCell(orNone(s)) }
func num(n int32) cli.Cell   { return cli.TextCell(fmt.Sprintf("%d", n)) }

func orNone(s string) string {
	if s == "" {
		return cli.None
	}
	return s
}

func i32ptrToIntPtr(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func gvkOf(kind string) schema.GroupVersionKind {
	return fleetv1.GroupVersion.WithKind(kind)
}

func init() {
	registerRobot()
	registerFleetAction()
	registerFleetZone()
	registerRobotClass()
	registerDiscoveredRobot()
	registerFleetAdapter()
	registerRobotProbe()
	registerFirmwareRollout()
	registerModelRollout()
	registerModelPolicy()
	registerSwarmadaConfig()
	registerZoneMaintenance()
	registerFleetTask()
}

func registerRobot() {
	registerResource(&resourceDef{
		singular: "robot", plural: "robots", kind: "Robot", aliases: []string{"rob"},
		gvk:       gvkOf("Robot"),
		newList:   func() client.ObjectList { return &fleetv1.RobotList{} },
		newObject: func() client.Object { return &fleetv1.Robot{} },
		items: func(l client.ObjectList) []client.Object {
			rl := l.(*fleetv1.RobotList)
			out := make([]client.Object, len(rl.Items))
			for i := range rl.Items {
				out[i] = &rl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "PHASE", cell: func(o client.Object) cli.Cell {
				return cli.RobotPhaseCell(string(o.(*fleetv1.Robot).Status.Phase))
			}},
			{header: "ZONE", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.Robot).Spec.Zone) }},
			{header: "BATTERY", cell: func(o client.Object) cli.Cell {
				r := o.(*fleetv1.Robot)
				return cli.BatteryCell(i32ptrToIntPtr(r.Status.BatteryPercent), r.Status.Phase == fleetv1.RobotPhaseCharging)
			}},
			{header: "TASK", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.Robot).Status.AssignedAction) }},
			{header: "CLASS", wide: true, cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.Robot).Spec.RobotClass) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell { return cli.TextCell(cli.Age(o.(*fleetv1.Robot).CreationTimestamp)) }},
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.Robot).Status.Conditions },
	})
}

func registerFleetAction() {
	registerResource(&resourceDef{
		// `fact` is FleetAction's own declared shortName
		// (api/v1/fleetaction_types.go: +kubebuilder:resource:shortName=fact). `ft`
		// was bound here in error and belongs to FleetTask.
		singular: "fleetaction", plural: "fleetactions", kind: "FleetAction", aliases: []string{"fact"},
		gvk:       gvkOf("FleetAction"),
		newList:   func() client.ObjectList { return &fleetv1.FleetActionList{} },
		newObject: func() client.Object { return &fleetv1.FleetAction{} },
		items: func(l client.ObjectList) []client.Object {
			tl := l.(*fleetv1.FleetActionList)
			out := make([]client.Object, len(tl.Items))
			for i := range tl.Items {
				out[i] = &tl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "TYPE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetAction).Spec.Type)) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetAction).Status.Phase)) }},
			{header: "PRIORITY", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetAction).Spec.Priority)) }},
			{header: "ROBOT", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FleetAction).Status.AssignedRobot) }},
			{header: "ZONE", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FleetAction).Spec.Zone) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.FleetAction).CreationTimestamp))
			}},
		},
		details: func(o client.Object) []cli.KV {
			t := o.(*fleetv1.FleetAction)
			return []cli.KV{
				{Label: "Type", Value: string(t.Spec.Type)},
				{Label: "Priority", Value: orNone(string(t.Spec.Priority))},
				{Label: "Zone", Value: orNone(t.Spec.Zone)},
				{Label: "Phase", Value: orNone(string(t.Status.Phase))},
				{Label: "Assigned Robot", Value: orNone(t.Status.AssignedRobot)},
				{Label: "Message", Value: orNone(t.Status.Message)},
			}
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.FleetAction).Status.Conditions },
	})
}

func registerFleetZone() {
	registerResource(&resourceDef{
		singular: "fleetzone", plural: "fleetzones", kind: "FleetZone", aliases: []string{"fz"},
		gvk:       gvkOf("FleetZone"),
		newList:   func() client.ObjectList { return &fleetv1.FleetZoneList{} },
		newObject: func() client.Object { return &fleetv1.FleetZone{} },
		items: func(l client.ObjectList) []client.Object {
			zl := l.(*fleetv1.FleetZoneList)
			out := make([]client.Object, len(zl.Items))
			for i := range zl.Items {
				out[i] = &zl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "ROBOTS", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FleetZone).Status.RobotCount) }},
			{header: "MAX", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FleetZone).Spec.MaxConcurrentRobots) }},
			{header: "ACTIVETASKS", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FleetZone).Status.ActiveActions) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell { return cli.TextCell(cli.Age(o.(*fleetv1.FleetZone).CreationTimestamp)) }},
		},
		details: func(o client.Object) []cli.KV {
			z := o.(*fleetv1.FleetZone)
			return []cli.KV{
				{Label: "Display Name", Value: orNone(z.Spec.DisplayName)},
				{Label: "Parent Zone", Value: orNone(z.Spec.ParentZone)},
				{Label: "Max Concurrent Robots", Value: fmt.Sprintf("%d", z.Spec.MaxConcurrentRobots)},
				{Label: "Robot Count", Value: fmt.Sprintf("%d", z.Status.RobotCount)},
				{Label: "Active Tasks", Value: fmt.Sprintf("%d", z.Status.ActiveActions)},
				{Label: "Is Leaf", Value: fmt.Sprintf("%t", z.Status.IsLeaf)},
			}
		},
	})
}

func registerRobotClass() {
	registerResource(&resourceDef{
		singular: "robotclass", plural: "robotclasses", kind: "RobotClass", aliases: []string{"rc"},
		gvk:       gvkOf("RobotClass"),
		newList:   func() client.ObjectList { return &fleetv1.RobotClassList{} },
		newObject: func() client.Object { return &fleetv1.RobotClass{} },
		items: func(l client.ObjectList) []client.Object {
			cl := l.(*fleetv1.RobotClassList)
			out := make([]client.Object, len(cl.Items))
			for i := range cl.Items {
				out[i] = &cl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "MANUFACTURER", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.RobotClass).Spec.Manufacturer) }},
			{header: "MODEL", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.RobotClass).Spec.Model) }},
			{header: "ADAPTER", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.RobotClass).Spec.BaseAdapter.Name) }},
			{header: "ROBOTS", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.RobotClass).Status.ReferencingRobots) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.RobotClass).CreationTimestamp))
			}},
		},
		details: func(o client.Object) []cli.KV {
			c := o.(*fleetv1.RobotClass)
			return []cli.KV{
				{Label: "Manufacturer", Value: c.Spec.Manufacturer},
				{Label: "Model", Value: c.Spec.Model},
				{Label: "Base Adapter", Value: fmt.Sprintf("%s (v%s)", c.Spec.BaseAdapter.Name, c.Spec.BaseAdapter.Version)},
				{Label: "Referencing Robots", Value: fmt.Sprintf("%d", c.Status.ReferencingRobots)},
			}
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.RobotClass).Status.Conditions },
	})
}

func registerDiscoveredRobot() {
	registerResource(&resourceDef{
		singular: "discoveredrobot", plural: "discoveredrobots", kind: "DiscoveredRobot", aliases: []string{"dr"},
		gvk:       gvkOf("DiscoveredRobot"),
		newList:   func() client.ObjectList { return &fleetv1.DiscoveredRobotList{} },
		newObject: func() client.Object { return &fleetv1.DiscoveredRobot{} },
		items: func(l client.ObjectList) []client.Object {
			dl := l.(*fleetv1.DiscoveredRobotList)
			out := make([]client.Object, len(dl.Items))
			for i := range dl.Items {
				out[i] = &dl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "MANUFACTURER", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.DiscoveredRobot).Status.Manufacturer) }},
			{header: "MODEL", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.DiscoveredRobot).Status.Model) }},
			{header: "FIRMWARE", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.DiscoveredRobot).Status.FirmwareVersion) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.DiscoveredRobot).Status.Phase)) }},
			{header: "CONNECTED", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.DateColumn(&o.(*fleetv1.DiscoveredRobot).Status.ConnectedAt))
			}},
			{header: "TTL", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.DateColumn(o.(*fleetv1.DiscoveredRobot).Status.TTLExpiresAt))
			}},
		},
		details: func(o client.Object) []cli.KV {
			d := o.(*fleetv1.DiscoveredRobot)
			return []cli.KV{
				{Label: "Manufacturer", Value: orNone(d.Status.Manufacturer)},
				{Label: "Model", Value: orNone(d.Status.Model)},
				{Label: "Firmware", Value: orNone(d.Status.FirmwareVersion)},
				{Label: "Phase", Value: orNone(string(d.Status.Phase))},
				{Label: "Adapter Address", Value: orNone(d.Status.AdapterAddress)},
				{Label: "Suggested Class", Value: orNone(d.Status.SuggestedRobotClass)},
				{Label: "Connected", Value: cli.DateColumn(&d.Status.ConnectedAt)},
				{Label: "TTL Expires", Value: cli.DateColumn(d.Status.TTLExpiresAt)},
			}
		},
	})
}

func registerFleetAdapter() {
	registerResource(&resourceDef{
		singular: "fleetadapter", plural: "fleetadapters", kind: "FleetAdapter", aliases: []string{"fa"},
		gvk:       gvkOf("FleetAdapter"),
		newList:   func() client.ObjectList { return &fleetv1.FleetAdapterList{} },
		newObject: func() client.Object { return &fleetv1.FleetAdapter{} },
		items: func(l client.ObjectList) []client.Object {
			al := l.(*fleetv1.FleetAdapterList)
			out := make([]client.Object, len(al.Items))
			for i := range al.Items {
				out[i] = &al.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "VENDOR", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FleetAdapter).Spec.Vendor) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetAdapter).Status.Phase)) }},
			{header: "CONFORMANCE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetAdapter).Status.Conformance)) }},
			{header: "ROBOTS", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FleetAdapter).Status.ConnectedRobots) }},
			{header: "ENDPOINT", wide: true, cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FleetAdapter).Spec.Endpoint) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.FleetAdapter).CreationTimestamp))
			}},
		},
		details: func(o client.Object) []cli.KV {
			a := o.(*fleetv1.FleetAdapter)
			return []cli.KV{
				{Label: "Vendor", Value: a.Spec.Vendor},
				{Label: "Endpoint", Value: a.Spec.Endpoint},
				{Label: "Phase", Value: orNone(string(a.Status.Phase))},
				{Label: "Conformance", Value: orNone(string(a.Status.Conformance))},
				{Label: "Connected Robots", Value: fmt.Sprintf("%d", a.Status.ConnectedRobots)},
				{Label: "Negotiated Protocol", Value: orNone(a.Status.NegotiatedProtocolVersion)},
				{Label: "Last Heartbeat", Value: cli.AgePtr(a.Status.LastHeartbeat)},
			}
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.FleetAdapter).Status.Conditions },
	})
}

func registerRobotProbe() {
	registerResource(&resourceDef{
		singular: "robotprobe", plural: "robotprobes", kind: "RobotProbe", aliases: []string{"rp"},
		gvk:       gvkOf("RobotProbe"),
		newList:   func() client.ObjectList { return &fleetv1.RobotProbeList{} },
		newObject: func() client.Object { return &fleetv1.RobotProbe{} },
		items: func(l client.ObjectList) []client.Object {
			pl := l.(*fleetv1.RobotProbeList)
			out := make([]client.Object, len(pl.Items))
			for i := range pl.Items {
				out[i] = &pl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "TYPE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.RobotProbe).Spec.ProbeType)) }},
			{header: "TARGET", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.RobotProbe).Spec.TargetComponent) }},
			{header: "RESULT", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.RobotProbe).Status.LastProbeResult)) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.RobotProbe).CreationTimestamp))
			}},
		},
		conditions: func(o client.Object) []metav1.Condition { return nil },
	})
}

func registerFirmwareRollout() {
	registerResource(&resourceDef{
		singular: "firmwarerollout", plural: "firmwarerollouts", kind: "FirmwareRollout", aliases: []string{"fwr"},
		gvk:       gvkOf("FirmwareRollout"),
		newList:   func() client.ObjectList { return &fleetv1.FirmwareRolloutList{} },
		newObject: func() client.Object { return &fleetv1.FirmwareRollout{} },
		items: func(l client.ObjectList) []client.Object {
			rl := l.(*fleetv1.FirmwareRolloutList)
			out := make([]client.Object, len(rl.Items))
			for i := range rl.Items {
				out[i] = &rl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "VERSION", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FirmwareRollout).Spec.NewVersion) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FirmwareRollout).Status.Phase)) }},
			{header: "UPDATED", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FirmwareRollout).Status.RobotsUpdated) }},
			{header: "TOTAL", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.FirmwareRollout).Status.RobotsTotal) }},
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.FirmwareRollout).Status.Conditions },
	})
}

func registerModelRollout() {
	registerResource(&resourceDef{
		singular: "modelrollout", plural: "modelrollouts", kind: "ModelRollout", aliases: []string{"mr"},
		gvk:       gvkOf("ModelRollout"),
		newList:   func() client.ObjectList { return &fleetv1.ModelRolloutList{} },
		newObject: func() client.Object { return &fleetv1.ModelRollout{} },
		items: func(l client.ObjectList) []client.Object {
			rl := l.(*fleetv1.ModelRolloutList)
			out := make([]client.Object, len(rl.Items))
			for i := range rl.Items {
				out[i] = &rl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "MODEL", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.ModelRollout).Spec.ModelName) }},
			{header: "VERSION", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.ModelRollout).Spec.NewVersion) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ModelRollout).Status.Phase)) }},
			{header: "UPDATED", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.ModelRollout).Status.RobotsUpdated) }},
			{header: "SUSPENDED", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.ModelRollout).Status.CapabilitiesSuspendedOn) }},
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.ModelRollout).Status.Conditions },
	})
}

func registerModelPolicy() {
	registerResource(&resourceDef{
		singular: "modelpolicy", plural: "modelpolicies", kind: "ModelPolicy", aliases: []string{"mp"},
		gvk:       gvkOf("ModelPolicy"),
		newList:   func() client.ObjectList { return &fleetv1.ModelPolicyList{} },
		newObject: func() client.Object { return &fleetv1.ModelPolicy{} },
		items: func(l client.ObjectList) []client.Object {
			pl := l.(*fleetv1.ModelPolicyList)
			out := make([]client.Object, len(pl.Items))
			for i := range pl.Items {
				out[i] = &pl.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "MODEL", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.ModelPolicy).Spec.ModelName) }},
			{header: "TRIGGER", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ModelPolicy).Spec.Trigger.Type)) }},
			{header: "AUTODEPLOY", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ModelPolicy).Spec.AutoDeployOn)) }},
			{header: "DEPLOYED", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.ModelPolicy).Status.DeploymentCount) }},
			{header: "REJECTED", cell: func(o client.Object) cli.Cell { return num(o.(*fleetv1.ModelPolicy).Status.RejectionCount) }},
			{header: "LASTDECISION", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ModelPolicy).Status.LastDecision)) }},
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.ModelPolicy).Status.Conditions },
	})
}

func registerSwarmadaConfig() {
	registerResource(&resourceDef{
		singular: "swarmadaconfig", plural: "swarmadaconfigs", kind: "SwarmadaConfig", aliases: []string{"sc"},
		gvk:       gvkOf("SwarmadaConfig"),
		newList:   func() client.ObjectList { return &fleetv1.SwarmadaConfigList{} },
		newObject: func() client.Object { return &fleetv1.SwarmadaConfig{} },
		items: func(l client.ObjectList) []client.Object {
			cl := l.(*fleetv1.SwarmadaConfigList)
			out := make([]client.Object, len(cl.Items))
			for i := range cl.Items {
				out[i] = &cl.Items[i]
			}
			return out
		},
		// SwarmadaConfig declares no printcolumns; kubectl's default view is
		// NAME + AGE, which we mirror.
		columns: []column{
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.SwarmadaConfig).CreationTimestamp))
			}},
		},
	})
}

func registerZoneMaintenance() {
	registerResource(&resourceDef{
		singular: "zonemaintenance", plural: "zonemaintenances", kind: "ZoneMaintenance", aliases: []string{"zm"},
		gvk:       gvkOf("ZoneMaintenance"),
		newList:   func() client.ObjectList { return &fleetv1.ZoneMaintenanceList{} },
		newObject: func() client.Object { return &fleetv1.ZoneMaintenance{} },
		items: func(l client.ObjectList) []client.Object {
			ml := l.(*fleetv1.ZoneMaintenanceList)
			out := make([]client.Object, len(ml.Items))
			for i := range ml.Items {
				out[i] = &ml.Items[i]
			}
			return out
		},
		columns: []column{
			{header: "SCOPE", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.ZoneMaintenance).Spec.Scope.ZoneName) }},
			{header: "MODE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ZoneMaintenance).Spec.Mode)) }},
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.ZoneMaintenance).Status.Phase)) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.ZoneMaintenance).CreationTimestamp))
			}},
		},
		details: func(o client.Object) []cli.KV {
			m := o.(*fleetv1.ZoneMaintenance)
			return []cli.KV{
				{Label: "Zone", Value: orNone(m.Spec.Scope.ZoneName)},
				{Label: "Mode", Value: orNone(string(m.Spec.Mode))},
				{Label: "Reason", Value: orNone(m.Spec.Reason)},
				{Label: "Phase", Value: orNone(string(m.Status.Phase))},
				{Label: "Activated By", Value: orNone(m.Status.ActivatedBy)},
			}
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.ZoneMaintenance).Status.Conditions },
	})
}

func registerFleetTask() {
	registerResource(&resourceDef{
		// `ft` is FleetTask's own declared shortName
		// (api/v1/fleettask_types.go: +kubebuilder:resource:shortName=ft), so
		// `kubectl get ft` and `swarmctl get ft` resolve to the same kind.
		singular: "fleettask", plural: "fleettasks", kind: "FleetTask", aliases: []string{"ft"},
		gvk:       gvkOf("FleetTask"),
		newList:   func() client.ObjectList { return &fleetv1.FleetTaskList{} },
		newObject: func() client.Object { return &fleetv1.FleetTask{} },
		items: func(l client.ObjectList) []client.Object {
			tl := l.(*fleetv1.FleetTaskList)
			out := make([]client.Object, len(tl.Items))
			for i := range tl.Items {
				out[i] = &tl.Items[i]
			}
			return out
		},
		// Mirrors the type's own printcolumn markers, in declared order.
		columns: []column{
			{header: "PHASE", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetTask).Status.Phase)) }},
			{header: "ACTIONS", cell: func(o client.Object) cli.Cell { return text(o.(*fleetv1.FleetTask).Status.ActionSummary) }},
			{header: "DESIRED", cell: func(o client.Object) cli.Cell { return text(string(o.(*fleetv1.FleetTask).Spec.DesiredState)) }},
			{header: "AGE", cell: func(o client.Object) cli.Cell {
				return cli.TextCell(cli.Age(o.(*fleetv1.FleetTask).CreationTimestamp))
			}},
		},
		details: func(o client.Object) []cli.KV {
			t := o.(*fleetv1.FleetTask)
			kv := []cli.KV{
				{Label: "Completion Policy", Value: orNone(string(t.Spec.CompletionPolicy))},
			}
			// Quorum is meaningful only under completionPolicy: Quorum, and a nil
			// quorum there is itself a defect worth surfacing rather than hiding
			// behind a default (ITEM-0050).
			if t.Spec.CompletionPolicy == fleetv1.CompletionPolicyQuorum {
				if t.Spec.Quorum == nil {
					kv = append(kv, cli.KV{Label: "Quorum", Value: "<unset>"})
				} else {
					kv = append(kv, cli.KV{Label: "Quorum", Value: fmt.Sprintf("%d", *t.Spec.Quorum)})
				}
			}
			return append(kv,
				cli.KV{Label: "Failure Policy", Value: orNone(string(t.Spec.FailurePolicy))},
				cli.KV{Label: "Desired State", Value: orNone(string(t.Spec.DesiredState))},
				cli.KV{Label: "Phase", Value: orNone(string(t.Status.Phase))},
				cli.KV{Label: "Actions", Value: fmt.Sprintf("%d declared, %s", len(t.Spec.Actions), orNone(t.Status.ActionSummary))},
				cli.KV{Label: "Started", Value: cli.AgePtr(t.Status.StartedAt)},
				cli.KV{Label: "Completed", Value: cli.AgePtr(t.Status.CompletionTime)},
			)
		},
		conditions: func(o client.Object) []metav1.Condition { return o.(*fleetv1.FleetTask).Status.Conditions },
	})
}
