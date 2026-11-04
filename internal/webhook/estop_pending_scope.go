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

package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// A zone's stop reaches the robots its fan-out finds in the zone and, when it propagates,
// in the zones below it (RFC-0001 #safety-estop-scopes). The fan-out reads the zone tree
// and the robots' zones when it runs, not when the stop is issued, so an edit that lands
// in between, taking a zone out from under the stopped zone or a robot out of it, leaves
// those robots outside the stop for good: the fan-out never finds them, and nothing
// records the stop on them. Such an edit is refused while the stop is issued and not yet
// applied: the zone carries swarmada.io/estop-triggered and no swarmada.io/estop-processed.
// Once the marker is written, the stop is recorded on every robot it reached
// (Robot.status.estopHeldBy) and holds them wherever the tree or the robot goes, so the edit
// is admitted again. If the stop is never applied (no control plane running), the edit stays
// refused: that fails closed.
//
// The zones are read through the uncached reader: a cached read can miss a trigger written
// a moment earlier and admit exactly the edit this refuses. A robot's own position report
// cannot be refused here; the Zone Controller holds that one (zone_controller.go).

// pendingStopsOver is estopscope.PendingStopsOver: the walk the Zone Controller's hold reads too.
func pendingStopsOver(ctx context.Context, rd client.Reader, namespace, start string, self bool) ([]string, error) {
	return estopscope.PendingStopsOver(ctx, rd, namespace, start, self)
}

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

func pendingStopRefusal(gr schema.GroupResource, name, what string, stops []string) error {
	return apierrors.NewForbidden(gr, name, fmt.Errorf(
		"%s would take it out from under zone %s, whose emergency stop is issued and not yet applied "+
			"(it carries swarmada.io/estop-triggered and no swarmada.io/estop-processed): the stop would then "+
			"never reach the robots it covers. Retry once the stop is applied; the zone then carries "+
			"swarmada.io/estop-processed, and the stop holds those robots wherever they go",
		what, quoteAll(stops)))
}

// refusePropagateChangeUnderPendingStop refuses a change of spec.estopPolicy.propagateToChildren
// while a stop that reaches the zone is issued and not yet applied: the zone's own stop, or the
// stop of an ancestor that propagates down to it. The fan-out reads the stopped zone's own policy
// when it runs, so on the stopped zone a change in that window changes which robots a stop
// already issued reaches. It reads no other zone's policy, so on a zone below the stopped zone the
// change alters no pending stop's reach; it is refused there too, and the message says so. The
// refusal covers both directions; the change is admitted again once the stop is applied.
func refusePropagateChangeUnderPendingStop(ctx context.Context, rd client.Reader, oldZone, newZone *fleetv1.FleetZone) error {
	before, after := propagatesToChildren(oldZone), propagatesToChildren(newZone)
	if before == after {
		return nil
	}
	stops, err := pendingStopsOver(ctx, rd, newZone.Namespace, newZone.Name, true)
	if err != nil || len(stops) == 0 {
		return err
	}
	if stops[0] != newZone.Name {
		return apierrors.NewForbidden(fleetZoneGR, newZone.Name, fmt.Errorf(
			"changing spec.estopPolicy.propagateToChildren of zone %q from %t to %t is refused while the "+
				"emergency stop of zone %s, which reaches it, is issued and not yet applied (it carries "+
				"swarmada.io/estop-triggered and no swarmada.io/estop-processed). That stop's fan-out reads only "+
				"the stopped zone's own propagateToChildren, so this change does not alter which robots it "+
				"reaches. Retry once the stop is applied; the stopped zone then carries swarmada.io/estop-processed",
			newZone.Name, before, after, quoteAll(stops)))
	}
	return apierrors.NewForbidden(fleetZoneGR, newZone.Name, fmt.Errorf(
		"changing spec.estopPolicy.propagateToChildren of zone %q from %t to %t would change which robots the "+
			"emergency stop of zone %q reaches, and that stop is issued and not yet applied (it carries "+
			"swarmada.io/estop-triggered and no swarmada.io/estop-processed): the stop would then reach other "+
			"robots than the ones it was issued over. Retry once the stop is applied; the zone then carries "+
			"swarmada.io/estop-processed, and the stop holds the robots it reached wherever they go",
		newZone.Name, before, after, newZone.Name))
}

// refuseEditThatUncoversAZone is the stop guard's one rule for a FleetZone edit: for the edited
// zone and every zone below it, the stops that cover a robot standing there are computed over
// the tree as it is and over the tree the edit would produce; an edit that loses any stop is
// refused, to every identity. A stop issued and not yet applied would never reach the robots
// there (pendingStopRefusal); a stop in force would end there without a clear (refuseNarrowing).
// The depth of every zone below the edited one is checked over the same tree, so a re-parent
// cannot put a zone past the bound the fan-out reaches (RFC-0001 #safety-estop-scopes).
func (v *FleetZoneValidator) refuseEditThatUncoversAZone(ctx context.Context, oldZone, newZone *fleetv1.FleetZone) error {
	reparented := oldZone.Spec.ParentZone != newZone.Spec.ParentZone
	narrowed := propagatesToChildren(oldZone) != propagatesToChildren(newZone)
	if !reparented && !narrowed {
		return nil
	}
	before, err := estopscope.ReadTree(ctx, v.APIReader, newZone.Namespace)
	if err != nil {
		return err
	}
	// The zone the API server holds, not the webhook's copy: the edit is judged against the
	// tree as it stands, with the edit applied on top. The carrier's own stop annotations are
	// the estop verbs' and the marker guard's to admit, so the edited zone keeps the stored
	// ones here: only the tree's shape is judged.
	if cur := before.Zone(oldZone.Name); cur != nil {
		oldZone = cur
	}
	edited := newZone.DeepCopy()
	edited.Annotations = oldZone.Annotations
	newZone = edited
	if reparented {
		if err := refuseDepthBeyondTheBound(before.With(newZone), newZone); err != nil {
			return err
		}
	}
	what := fmt.Sprintf("re-parenting zone %q from %q to %q", newZone.Name, oldZone.Spec.ParentZone, newZone.Spec.ParentZone)
	if !reparented {
		what = fmt.Sprintf("setting spec.estopPolicy.propagateToChildren of zone %q to %t", newZone.Name, propagatesToChildren(newZone))
	}
	loss, err := estopscope.EditUncovers(before, newZone)
	if err != nil {
		// A tree already past the bound (two edits admitted together can leave one) cannot be
		// judged for what the edit loses. With no zone stop issued or in force there is nothing
		// to lose, and the edit is admitted if it leaves this zone's subtree inside the bound
		// (checked above), so the tree can be repaired. With a stop standing it is refused.
		var deep *estopscope.TooDeepError
		if errors.As(err, &deep) && before.StopCarrier() == "" {
			return nil
		}
		return err
	}
	if len(loss.Pending) > 0 {
		return pendingStopRefusal(fleetZoneGR, newZone.Name, what, loss.Pending)
	}
	return refuseNarrowing(newZone.Namespace, newZone.Name, what, loss.InForce)
}

// refuseDepthBeyondTheBound refuses an edit that would put the zone, or any zone below it, more
// than the admitted number of ancestors deep: a stop on the root would not reach a robot there.
func refuseDepthBeyondTheBound(after *estopscope.Tree, zone *fleetv1.FleetZone) error {
	deepest, name, err := after.DeepestBelow(zone.Name)
	if err != nil {
		return apierrors.NewInvalid(fleetZoneGK, zone.Name, field.ErrorList{field.Invalid(
			field.NewPath("spec").Child("parentZone"), zone.Spec.ParentZone, err.Error())})
	}
	if deepest > estopscope.MaxAncestors {
		return apierrors.NewInvalid(fleetZoneGK, zone.Name, field.ErrorList{field.Invalid(
			field.NewPath("spec").Child("parentZone"), zone.Spec.ParentZone,
			tooDeepMessage(name, deepest))})
	}
	return nil
}

// tooDeepMessage names a zone that would have more ancestors than the hierarchy admits.
func tooDeepMessage(zone string, depth int) string {
	return fmt.Sprintf("zone %q would have %d ancestors; the zone hierarchy admits a zone and %d ancestors, the depth a "+
		"zone's emergency stop reaches", zone, depth, estopscope.MaxAncestors)
}

// refuseNarrowing refuses what when stops names any zone whose stop it would narrow.
func refuseNarrowing(namespace, name, what string, stops []string) error {
	if len(stops) == 0 {
		return nil
	}
	return apierrors.NewForbidden(fleetZoneGR, name, fmt.Errorf(
		"%s would take zones out of the reach of the emergency stop of zone %s, which is in force: a robot "+
			"that later enters them would not be stopped, which would end the stop there without a clear. A "+
			"stop is released only through its clear: clear it first, which the estop-clear verb authorizes "+
			"(swarmctl estop-clear %s --reason <text> -n %s), and retry once the clear is applied",
		what, quoteAll(stops), stops[0], namespace))
}

// propagatesToChildren is estopPolicy.propagateToChildren with its default (true) applied, as
// the fan-out reads it.
func propagatesToChildren(z *fleetv1.FleetZone) bool {
	return z.Spec.EstopPolicy == nil || z.Spec.EstopPolicy.PropagatesToChildren()
}

// refuseMoveOutOfPendingStop refuses a spec.zone change that takes a robot out of the reach of
// a stop it does not record: one still being applied (the fan-out would never find it), or one
// in force that has not reached it yet (it came under the stop a moment ago, and the pass that
// stops and records it has not run). A stop the robot records travels with it, so a recorded
// robot moves freely (the stop guard, estopscope).
func refuseMoveOutOfPendingStop(ctx context.Context, rd client.Reader, oldRobot, newRobot *fleetv1.Robot) error {
	if oldRobot.Spec.Zone == newRobot.Spec.Zone || oldRobot.Spec.Zone == "" {
		return nil
	}
	tree, configs, err := estopscope.ReadWorld(ctx, rd, newRobot.Namespace)
	if err != nil {
		return err
	}
	moved := oldRobot.DeepCopy()
	moved.Spec.Zone = newRobot.Spec.Zone
	lostStops, err := estopscope.MoveLoses(tree, configs, oldRobot, moved)
	if err != nil {
		return err
	}
	var pending, inForce []string
	for _, lost := range lostStops {
		if lost.Pending {
			pending = append(pending, lost.Name)
		} else {
			inForce = append(inForce, lost.Name)
		}
	}
	what := fmt.Sprintf("moving robot %q from zone %q to %q", newRobot.Name, oldRobot.Spec.Zone, newRobot.Spec.Zone)
	if len(pending) > 0 {
		return pendingStopRefusal(robotGR, newRobot.Name, what, pending)
	}
	if len(inForce) > 0 {
		return apierrors.NewForbidden(robotGR, newRobot.Name, fmt.Errorf(
			"%s would take it out from under zone %s, whose emergency stop is in force and has not yet reached the robot: "+
				"the stop reaches a robot where it stands, and the robot would leave before it does. Retry once the robot "+
				"records the stop (status.estopHeldBy); the stop then holds it wherever it goes",
			what, quoteAll(inForce)))
	}
	return nil
}
