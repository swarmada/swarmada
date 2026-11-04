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

package estopscope

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The stop guard. One answer to "which stops cover this robot", read from the API server,
// over the robot's recorded zone and its declared zone, each with up to walkCap ancestors,
// the robot's own trigger, its recorded holds and the namespace's SwarmadaConfigs. A robot
// being deleted is a robot. A chain still climbing past the bound, or a read error, is an
// error, and every caller treats an error as a hold or a refusal, never as "not covered".
// The fan-out, the late-arrival pass, dispatch, the Zone Controller's hold and every
// admission check that moves a robot or edits the zone tree read their answer here, so
// they cannot disagree (RFC-0001 #safety-estop-scopes).

// MaxAncestors is the deepest zone the hierarchy admits: a zone and MaxAncestors ancestors.
// Admission refuses a tree deeper than this, so the walk's bound is never reached by a
// legal tree.
const MaxAncestors = walkCap

// Tree is the zone tree of one namespace as it was read at one moment.
type Tree struct {
	byName map[string]*fleetv1.FleetZone
}

// ReadTree lists the namespace's zones through rd, which must read the API server.
func ReadTree(ctx context.Context, rd client.Reader, namespace string) (*Tree, error) {
	if rd == nil {
		return nil, fmt.Errorf("no uncached reader is configured to read the zones of namespace %q", namespace)
	}
	var zones fleetv1.FleetZoneList
	if err := rd.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing the zones of namespace %q: %w", namespace, err)
	}
	return TreeOf(zones.Items), nil
}

// TreeOf is the tree formed by zones.
func TreeOf(zones []fleetv1.FleetZone) *Tree {
	t := &Tree{byName: make(map[string]*fleetv1.FleetZone, len(zones))}
	for i := range zones {
		t.byName[zones[i].Name] = &zones[i]
	}
	return t
}

// Zone returns the named zone, or nil.
func (t *Tree) Zone(name string) *fleetv1.FleetZone {
	if t == nil {
		return nil
	}
	return t.byName[name]
}

// Names returns every zone name, sorted.
func (t *Tree) Names() []string {
	out := make([]string, 0, len(t.byName))
	for n := range t.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// With is the tree with z in place of the zone of its name: the tree an edit would produce.
func (t *Tree) With(z *fleetv1.FleetZone) *Tree {
	n := &Tree{byName: make(map[string]*fleetv1.FleetZone, len(t.byName)+1)}
	for k, v := range t.byName {
		n.byName[k] = v
	}
	n.byName[z.Name] = z
	return n
}

// Without is the tree with the named zone removed: the tree a delete would produce.
func (t *Tree) Without(name string) *Tree {
	n := &Tree{byName: make(map[string]*fleetv1.FleetZone, len(t.byName))}
	for k, v := range t.byName {
		if k != name {
			n.byName[k] = v
		}
	}
	return n
}

// StopCarrier names a zone in the tree that carries a stop, issued or in force, or "" when
// none does.
func (t *Tree) StopCarrier() string {
	for _, name := range t.Names() {
		if Carries(t.byName[name]) {
			return name
		}
	}
	return ""
}

// TooDeepError reports a parentZone chain that climbs past MaxAncestors ancestors from a zone.
type TooDeepError struct {
	Zone string
	// Depth is how many ancestors were walked before the bound was reached.
	Depth int
}

func (e *TooDeepError) Error() string {
	return fmt.Sprintf("the parentZone chain above zone %q is deeper than %d zones; it cannot be checked for a stop",
		e.Zone, MaxAncestors)
}

// Ancestors returns the named zone's ancestors, nearest first, up to MaxAncestors. A zone the
// tree does not hold ends the chain, as it ends the fan-out's. A chain still climbing past the
// bound, or one that revisits a zone, is a TooDeepError: the caller fails closed.
func (t *Tree) Ancestors(zone string) ([]*fleetv1.FleetZone, error) {
	var out []*fleetv1.FleetZone
	seen := map[string]bool{zone: true}
	z := t.Zone(zone)
	if z == nil {
		return nil, nil
	}
	name := z.Spec.ParentZone
	for depth := 0; name != ""; depth++ {
		if depth >= MaxAncestors || seen[name] {
			return nil, &TooDeepError{Zone: zone, Depth: depth}
		}
		seen[name] = true
		p := t.Zone(name)
		if p == nil {
			break
		}
		out = append(out, p)
		name = p.Spec.ParentZone
	}
	return out, nil
}

// Depth is the number of ancestors the named zone has; a zone missing from the tree has none.
func (t *Tree) Depth(zone string) (int, error) {
	a, err := t.Ancestors(zone)
	return len(a), err
}

// Reaching returns the names of the zones whose stop, as stopped selects it, reaches a robot
// standing in zone: the zone itself whatever its policy, and every ancestor whose stop
// propagates down. It is the walk the fan-out, dispatch and admission share; it fails closed
// past the bound.
func (t *Tree) Reaching(zone string, stopped func(*fleetv1.FleetZone) bool) ([]string, error) {
	z := t.Zone(zone)
	if z == nil {
		return nil, nil
	}
	var out []string
	if stopped(z) {
		out = append(out, z.Name)
	}
	ancestors, err := t.Ancestors(zone)
	if err != nil {
		return nil, err
	}
	for _, a := range ancestors {
		if stopped(a) && propagatesDown(a) {
			out = append(out, a.Name)
		}
	}
	return out, nil
}

// Covers reports whether a stop on carrier reaches a robot standing in zone: always when zone
// is the carrier; when toChildren, when the carrier is an ancestor of zone. It is the fan-out's
// question, bounded and fail-closed.
func (t *Tree) Covers(carrier, zone string, toChildren bool) (bool, error) {
	if zone == "" {
		return false, nil
	}
	if zone == carrier {
		return true, nil
	}
	if !toChildren {
		return false, nil
	}
	ancestors, err := t.Ancestors(zone)
	if err != nil {
		return false, err
	}
	for _, a := range ancestors {
		if a.Name == carrier {
			return true, nil
		}
	}
	return false, nil
}

// FanOutScope reports, for each zone in zones (a robot's recorded zone, else its declared
// one), whether the zone fan-out of carrier's stop reaches a robot standing there (Covers).
// A zone whose chain cannot be walked (past the bound, or a cycle: two zone edits admitted
// together can leave one) is in scope, and its index is listed in undecided: whether the stop
// reaches it cannot be decided, and a stop that cannot be fanned out completely stops more
// robots, never fewer. Failing the pass instead would stop no robot at all.
//
// err is reserved for a question the tree cannot answer at all; a chain it cannot walk is not one.
func (t *Tree) FanOutScope(carrier string, zones []string, toChildren bool) (in []bool, undecided []int, err error) {
	in = make([]bool, len(zones))
	for i, z := range zones {
		ok, cerr := t.Covers(carrier, z, toChildren)
		if cerr != nil {
			ok = true
			undecided = append(undecided, i)
		}
		in[i] = ok
	}
	return in, undecided, nil
}

// Subtree returns the named zone and every zone below it, in breadth-first order, each with
// its depth below the root. A tree that cycles below the root is a TooDeepError.
func (t *Tree) Subtree(root string) ([]string, error) {
	children := map[string][]string{}
	for _, name := range t.Names() {
		if p := t.byName[name].Spec.ParentZone; p != "" {
			children[p] = append(children[p], name)
		}
	}
	out := []string{root}
	seen := map[string]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i]] {
			if seen[c] {
				return nil, &TooDeepError{Zone: c, Depth: MaxAncestors}
			}
			seen[c] = true
			out = append(out, c)
		}
		if len(out) > len(t.byName)+1 {
			return nil, &TooDeepError{Zone: root, Depth: MaxAncestors}
		}
	}
	return out, nil
}

// DeepestBelow returns the greatest number of ancestors any zone at or below root has in this
// tree, and that zone's name.
func (t *Tree) DeepestBelow(root string) (int, string, error) {
	sub, err := t.Subtree(root)
	if err != nil {
		return 0, "", err
	}
	deepest, name := -1, root
	for _, z := range sub {
		d, err := t.Depth(z)
		if err != nil {
			return 0, "", err
		}
		if d > deepest {
			deepest, name = d, z
		}
	}
	return deepest, name, nil
}

// InForce reports whether a carrier's annotations hold a stop in force or pending: the trigger,
// or the processed marker that stands until the clear has been applied.
func InForce(annotations map[string]string) bool {
	_, triggered := annotations[TriggerAnnotation]
	_, processed := annotations[ProcessedAnnotation]
	return triggered || processed
}

// CoveringAt returns the stops that cover a robot standing in zone: the zone stops Reaching finds
// (with Carries) and every namespace stop. Each Holder's Pending says whether the stop is
// issued and not yet applied. A zone-less position (zone "") is covered by the namespace only.
func CoveringAt(t *Tree, configs []fleetv1.SwarmadaConfig, zone string) ([]Holder, error) {
	var out []Holder
	if zone != "" {
		names, err := t.Reaching(zone, Carries)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			z := t.Zone(n)
			out = append(out, Holder{Kind: KindZone, Name: n, Value: z.Annotations[TriggerAnnotation], Pending: Pending(z)})
		}
	}
	for i := range configs {
		if InForce(configs[i].Annotations) {
			out = append(out, Holder{Kind: KindNamespace, Name: configs[i].Name,
				Value: configs[i].Annotations[TriggerAnnotation], Pending: pendingOn(configs[i].Annotations)})
		}
	}
	return out, nil
}

// Coverage is the stops that cover one robot now.
type Coverage struct {
	Stops []Holder
}

// Covered reports whether any stop covers the robot.
func (c Coverage) Covered() bool { return len(c.Stops) > 0 }

// Pending reports whether any covering stop is issued and not yet applied.
func (c Coverage) Pending() bool {
	for _, s := range c.Stops {
		if s.Pending {
			return true
		}
	}
	return false
}

// Names returns "kind/name" for every covering stop, in order.
func (c Coverage) Names() []string {
	out := make([]string, 0, len(c.Stops))
	for _, s := range c.Stops {
		out = append(out, string(s.Kind)+"/"+s.Name)
	}
	return out
}

// Has reports whether a stop of the given kind and name covers the robot.
func (c Coverage) Has(kind Kind, name string) bool {
	for _, s := range c.Stops {
		if s.Kind == kind && s.Name == name {
			return true
		}
	}
	return false
}

// Lost returns the stops in before that are not in after: what an edit would take a robot out of.
func Lost(before, after Coverage) []Holder {
	var out []Holder
	for _, s := range before.Stops {
		if !after.Has(s.Kind, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

// CoverageOf is the stop guard's answer for one robot over a tree and configs already read:
// the robot's own trigger, the zone stops over its recorded zone and its declared zone, the
// namespace stops, and every recorded hold (in force, or orphaned: its carrier is gone or
// exists under another UID). The robot's deletion timestamp plays no part.
func CoverageOf(t *Tree, configs []fleetv1.SwarmadaConfig, robot *fleetv1.Robot) (Coverage, error) {
	var cov Coverage
	seen := map[string]bool{}
	add := func(h Holder) {
		key := string(h.Kind) + "/" + h.Name
		if !seen[key] {
			seen[key] = true
			cov.Stops = append(cov.Stops, h)
		}
	}
	if v, ok := robot.Annotations[TriggerAnnotation]; ok {
		add(Holder{Kind: KindRobot, Name: robot.Name, Value: v, Pending: pendingOn(robot.Annotations)})
	}
	for _, zone := range []string{robot.Status.CurrentZone, robot.Spec.Zone} {
		if zone == "" {
			continue
		}
		at, err := CoveringAt(t, nil, zone)
		if err != nil {
			return Coverage{}, err
		}
		for _, h := range at {
			add(h)
		}
	}
	ns, _ := CoveringAt(t, configs, "")
	for _, h := range ns {
		add(h)
	}
	cfgByName := make(map[string]*fleetv1.SwarmadaConfig, len(configs))
	for i := range configs {
		cfgByName[configs[i].Name] = &configs[i]
	}
	for _, e := range robot.Status.EstopHeldBy {
		h := Holder{Kind: HoldKind(e.Kind), Name: e.Name, Recorded: true}
		var uid string
		var ann map[string]string
		found := false
		switch e.Kind {
		case fleetv1.EstopHoldSwarmadaConfig:
			if c, ok := cfgByName[e.Name]; ok {
				uid, ann, found = string(c.UID), c.Annotations, true
			}
		default:
			if z := t.Zone(e.Name); z != nil {
				uid, ann, found = string(z.UID), z.Annotations, true
			}
		}
		switch {
		case !found || (e.UID != "" && uid != "" && e.UID != uid):
			h.Orphaned = true
			add(h)
		case InForce(ann):
			h.Value = ann[TriggerAnnotation]
			add(h)
		}
	}
	return cov, nil
}

// EditLoss is what a FleetZone edit would take out of a stop's reach: the names of the stops,
// pending and in force, that cover a robot standing in the edited zone or in any zone below it
// before the edit and not after.
type EditLoss struct {
	Pending []string
	InForce []string
}

// Any reports whether the edit loses a stop.
func (l EditLoss) Any() bool { return len(l.Pending)+len(l.InForce) > 0 }

// EditUncovers is the stop guard's rule for a FleetZone edit: over before, the tree as it
// stands, and the tree the edit produces (newZone in place of the zone of its name), the stops
// covering a robot standing in the edited zone or in any zone below it are compared, and every
// stop lost is named. A tree the walk cannot finish is an error; the caller refuses.
func EditUncovers(before *Tree, newZone *fleetv1.FleetZone) (EditLoss, error) {
	var loss EditLoss
	after := before.With(newZone)
	sub, err := before.Subtree(newZone.Name)
	if err != nil {
		return loss, err
	}
	seen := map[string]bool{}
	for _, d := range sub {
		was, err := CoveringAt(before, nil, d)
		if err != nil {
			return loss, err
		}
		now, err := CoveringAt(after, nil, d)
		if err != nil {
			return loss, err
		}
		for _, lost := range Lost(Coverage{Stops: was}, Coverage{Stops: now}) {
			if seen[lost.Name] {
				continue
			}
			seen[lost.Name] = true
			if lost.Pending {
				loss.Pending = append(loss.Pending, lost.Name)
			} else {
				loss.InForce = append(loss.InForce, lost.Name)
			}
		}
	}
	return loss, nil
}

// FanOutCoverageOf is the fan-out's reading of the same question: the zone stops over the
// robot's recorded zone, or its declared zone when no zone is recorded (robotsInEstopScope reads
// one or the other, not both), the namespace stops, the robot's own trigger and its records.
// CoverageOf is the cautious superset the holds and refusals use; a move loses a stop when it
// is lost under either reading (MoveLoses).
func FanOutCoverageOf(t *Tree, configs []fleetv1.SwarmadaConfig, robot *fleetv1.Robot) (Coverage, error) {
	one := robot.DeepCopy()
	if one.Status.CurrentZone != "" {
		one.Spec.Zone = one.Status.CurrentZone
	}
	return CoverageOf(t, configs, one)
}

// MoveLoses returns the stops a robot would lose by becoming after instead of before, under the
// cautious reading and under the fan-out's reading: a stop lost under either is lost. A stop the
// robot records is never lost, because the record travels with it.
func MoveLoses(t *Tree, configs []fleetv1.SwarmadaConfig, before, after *fleetv1.Robot) ([]Holder, error) {
	var out []Holder
	seen := map[string]bool{}
	for _, read := range []func(*Tree, []fleetv1.SwarmadaConfig, *fleetv1.Robot) (Coverage, error){CoverageOf, FanOutCoverageOf} {
		was, err := read(t, configs, before)
		if err != nil {
			return nil, err
		}
		now, err := read(t, configs, after)
		if err != nil {
			return nil, err
		}
		for _, l := range Lost(was, now) {
			key := string(l.Kind) + "/" + l.Name
			if !seen[key] {
				seen[key] = true
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// Covering is CoverageOf over a fresh read of the namespace's zones and configs through rd,
// which must read the API server.
func Covering(ctx context.Context, rd client.Reader, robot *fleetv1.Robot) (Coverage, error) {
	t, configs, err := ReadWorld(ctx, rd, robot.Namespace)
	if err != nil {
		return Coverage{}, err
	}
	return CoverageOf(t, configs, robot)
}

// ReadWorld reads the namespace's zone tree and its SwarmadaConfigs through rd.
func ReadWorld(ctx context.Context, rd client.Reader, namespace string) (*Tree, []fleetv1.SwarmadaConfig, error) {
	t, err := ReadTree(ctx, rd, namespace)
	if err != nil {
		return nil, nil, err
	}
	var configs fleetv1.SwarmadaConfigList
	if err := rd.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing the SwarmadaConfigs of namespace %q: %w", namespace, err)
	}
	return t, configs.Items, nil
}

// PendingStopsOverTree is PendingStopsOver over a tree already read: the zones whose pending
// stop reaches a robot standing in zone.
func PendingStopsOverTree(t *Tree, zone string) ([]string, error) {
	return t.Reaching(zone, Pending)
}
