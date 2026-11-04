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

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// `occupantAction: Report` — the ONE observable thing it does (ADR-0058 §10).
//
// Report is the DEFAULT. Until this producer existed, all three values of
// occupantAction did nothing at all, which made Report indistinguishable from the two
// the admission webhook warns are inert — and made that warning harder to read rather
// than easier, because it had to disclaim a difference that was not there.
//
// THREE CONSTRAINTS, AND EACH IS WHERE THIS GOES WRONG:
//
//  1. ONE event when the denial TAKES EFFECT, not one per reconcile. Latched exactly
//     like the RestrictionContradiction event beside it (warnOnContradiction), against
//     the same mutex and with the same "was it already true" test — a second mechanism
//     would be a second thing to keep in step.
//  2. NEVER ON A TELEMETRY TICK (RA-1). The occupant list is the trap: it is about
//     position, and position is precisely what may not drive a write or an emit. It is
//     read from PUBLISHED projections — Robot.status.containingZones[] and
//     status.currentZone, each written only on a material transition — never derived
//     from a frame.
//  3. IT NAMES WHO WAS PRESENT AND CLAIMS NOTHING MORE. Not that they left, not that
//     they were stopped, not that they will be. Report moves no robot, and the event
//     must not be readable as saying it did.

// occupantZone builds a zone with geometry and, optionally, a declared entry denial
// plus the matching resolved projection — as the Zone Controller leaves it.
func occupantZone(name string, pts []fleetv1.Point, denyReason string, occupant fleetv1.ZoneOccupantAction) *fleetv1.FleetZone {
	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetZoneSpec{
			PhysicalBounds: &fleetv1.PhysicalBounds{Floor: 0, Polygon: pts},
		},
	}
	if denyReason != "" {
		fz.Spec.Restrictions = &fleetv1.ZoneRestrictions{
			Entry: &fleetv1.ZoneEntryRestriction{
				Mode: fleetv1.ZoneRestrictionDenied, Reason: denyReason, OccupantAction: occupant,
			},
		}
	}
	return fz
}

// occupantRobot is a robot published as being inside zoneName, by BOTH projections the
// producer may read: the arbitrated currentZone and the containment set. Both are
// material-transition products, which is what keeps this off the telemetry path.
func occupantRobot(name, robotID, zoneName string) *fleetv1.Robot {
	r := robotAt(name, robotID, zoneName)
	r.Status.ContainingZones = []string{zoneName}
	return r
}

// drainEvents returns every event the recorder currently holds.
func drainEvents(t *testing.T, z *ZoneController) []string {
	t.Helper()
	rec, ok := z.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("recorder is not a FakeRecorder")
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func occupantEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if strings.Contains(e, zoneReasonEntryDeniedWithOccupants) {
			out = append(out, e)
		}
	}
	return out
}

var patioPts = []fleetv1.Point{{X: 0, Y: 0}, {X: 100, Y: 0}, {X: 100, Y: 100}, {X: 0, Y: 100}}

// THE DENIAL TAKES EFFECT OVER OCCUPANTS, AND THE EVENT NAMES THEM.
func TestOccupantReport_TheDenialTakingEffectNamesTheRobotsPresent(t *testing.T) {
	ctx := context.Background()
	z, c := newZoneController(t,
		occupantZone("patio", patioPts, "", fleetv1.ZoneOccupantReport),
		occupantRobot("amr-1", "rid1", "patio"),
		occupantRobot("amr-2", "rid2", "patio"),
	)

	// No entry rule yet: nothing to report.
	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("a zone with no entry denial emitted %v", got)
	}

	// The operator closes the zone while two robots are standing in it.
	var fz fleetv1.FleetZone
	if err := c.Get(ctx, nsName("patio"), &fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	fz.Spec.Restrictions = &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{
			Mode: fleetv1.ZoneRestrictionDenied, Reason: "outdoors behind the fence",
			OccupantAction: fleetv1.ZoneOccupantReport,
		},
	}
	fz.Generation++ // a spec write advances it; the fake client does not
	if err := c.Update(ctx, &fz); err != nil {
		t.Fatalf("update zone: %v", err)
	}
	reconcileZone(t, z, "patio")

	got := occupantEvents(drainEvents(t, z))
	if len(got) != 1 {
		t.Fatalf("%d ZoneEntryDeniedWithOccupants events, want exactly 1: %v.\n"+
			"occupantAction: Report is the DEFAULT, and until this event exists it does "+
			"NOTHING observable — indistinguishable from the two modes the admission webhook "+
			"warns are inert, which makes that warning harder to read rather than easier. "+
			"Three normative texts described this event and no producer emitted it", len(got), got)
	}
	e := got[0]
	for _, want := range []string{"amr-1", "amr-2", "patio", "outdoors behind the fence"} {
		if !strings.Contains(e, want) {
			t.Errorf("event %q does not name %q. The event's whole content is WHO WAS PRESENT "+
				"and why the zone closed; an operator who cannot tell which robots were inside "+
				"has been told less than nothing", e, want)
		}
	}
	// IT CLAIMS NOTHING MORE. Report moves no robot, and the sentence must not be
	// readable as saying it did.
	//
	// ASSERTED AS THE PRESENCE OF THE EXPLICIT DENIAL, AND NOT AS A BAN ON THE MOTION
	// VOCABULARY — because a substring test cannot do the second thing, and two attempts
	// to write one are recorded here so a third is not attempted.
	//
	// The first version banned "wound down" and "requeued" and went red on the message's
	// own negation of them. The second banned the affirmative forms — "has been moved",
	// "have left" — and went red on "NO ROBOT HAS BEEN MOVED" and "not a statement that
	// they have left". **A substring check cannot distinguish a claim from its denial**,
	// and both failures were the test being wrong about a message that says exactly the
	// right thing. Pushed further, such a check forces the message to say LESS about what
	// did not happen, which is the opposite of what this field needs — the whole reason
	// Report defaults is that the plane does not move robots on a spec edit, and that has
	// to be said out loud rather than left as an absence.
	//
	// So what is asserted is the DENIAL, positively. Whether the surrounding prose also
	// implies motion is a judgement a human review makes on the string, and this comment
	// is where the requirement is written down for that reviewer.
	if !strings.Contains(e, "NO ROBOT HAS BEEN MOVED") {
		t.Errorf("event %q does not state that no robot was moved. Report is the default "+
			"BECAUSE the plane does not move robots on a spec edit; an event that leaves that "+
			"to be inferred reintroduces the expectation this field's docs were struck for", e)
	}
	// Past tense about PRESENCE. "were inside" is a fact about the moment the denial took
	// effect; "are inside" would be a live occupancy claim, which ADR-0058 §10 forbids
	// this event from becoming.
	if !strings.Contains(e, "were inside") {
		t.Errorf("event %q does not say the robots WERE inside. The event reports a moment, "+
			"not a live occupancy list", e)
	}
}

// ONE EVENT PER TRANSITION, NOT ONE PER RECONCILE — latched like the contradiction
// event beside it. An event stream that repeats an unchanged fact is one an operator
// stops reading, which costs the next real one.
func TestOccupantReport_ResyncsDoNotReEmit(t *testing.T) {
	z, _ := newZoneController(t,
		occupantZone("patio", patioPts, "outdoors behind the fence", fleetv1.ZoneOccupantReport),
		occupantRobot("amr-1", "rid1", "patio"),
	)

	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 1 {
		t.Fatalf("%d events on the transition into denied-with-occupants, want 1: %v", len(got), got)
	}
	for i := 0; i < 4; i++ {
		reconcileZone(t, z, "patio")
	}
	if got := occupantEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("%d further events across four resyncs, want 0: %v. The event is specified as "+
			"ONE event when the denial takes effect; a per-reconcile emit is a stream", len(got), got)
	}
}

// RA-1 — THE PROOF, in the shape L8's containment-push test established. A telemetry
// tick that changes nothing writes nothing and emits nothing.
//
// Asserted on the ZONE's resourceVersion as well as the event count: the occupant list
// is about position, and a producer that recomputed it per frame would show up as either
// a status write or an emit. Neither may move.
func TestOccupantReport_AnUnchangedTickWritesNothingAndEmitsNothing(t *testing.T) {
	ctx := context.Background()
	z, c := newZoneController(t,
		occupantZone("patio", patioPts, "outdoors behind the fence", fleetv1.ZoneOccupantReport),
		occupantRobot("amr-1", "rid1", "patio"),
	)
	z.setZones(actionNS, []zone.Candidate{
		{Name: "patio", Floor: 0, Polygon: toZonePoints(patioPts), IsLeaf: true, Depth: 0},
	})

	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 1 {
		t.Fatalf("%d events on the transition, want 1", len(got))
	}
	zoneRV := getZoneRV(t, c, "patio")
	robotRV := getRobotRV(t, c, "amr-1")

	// Five identical position frames, all inside the polygon. Containment does not move.
	inside := &telemetry.Position{X: 50, Y: 50, Floor: 0}
	for i := 0; i < 5; i++ {
		z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	}

	if got := getRobotRV(t, c, "amr-1"); got != robotRV {
		t.Fatalf("the ROBOT's resourceVersion moved %q -> %q across five identical frames (RA-1)",
			robotRV, got)
	}
	if got := getZoneRV(t, c, "patio"); got != zoneRV {
		t.Fatalf("the ZONE's resourceVersion moved %q -> %q across five identical frames. The "+
			"occupant list is derived from POSITION, which is exactly where a per-tick write is "+
			"prohibited — and where a producer that recomputed it per frame would surface",
			zoneRV, got)
	}
	if got := occupantEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("%d occupant events across five identical frames, want 0: %v. A tick that "+
			"changes nothing writes nothing AND emits nothing", len(got), got)
	}
}

// A DENIAL WITH NOBODY INSIDE EMITS NOTHING. The event is about occupants; with none
// there is nothing to report, and an event saying "0 robots" is noise on every zone an
// operator closes ahead of time.
func TestOccupantReport_ADenialWithNoOccupantsIsSilent(t *testing.T) {
	z, _ := newZoneController(t,
		occupantZone("patio", patioPts, "outdoors behind the fence", fleetv1.ZoneOccupantReport),
		occupantRobot("amr-1", "rid1", "warehouse"), // somewhere else
	)
	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("%d events for a denial with nobody inside, want 0: %v", len(got), got)
	}
}

// THE OTHER TWO MODES REPORT TOO, and that is deliberate rather than an oversight.
// Graceful and Immediate are RESERVED: they move no robot at v0.4, so the zone behaves
// as Report — which is exactly what the admission webhook tells the operator. If they
// emitted nothing, the webhook's "behaves as Report" would be false again in the other
// direction.
func TestOccupantReport_ReservedModesReportBecauseTheyBehaveAsReport(t *testing.T) {
	for _, mode := range []fleetv1.ZoneOccupantAction{
		fleetv1.ZoneOccupantGraceful, fleetv1.ZoneOccupantImmediate,
	} {
		t.Run(string(mode), func(t *testing.T) {
			z, _ := newZoneController(t,
				occupantZone("patio", patioPts, "outdoors behind the fence", mode),
				occupantRobot("amr-1", "rid1", "patio"),
			)
			reconcileZone(t, z, "patio")
			got := occupantEvents(drainEvents(t, z))
			if len(got) != 1 {
				t.Fatalf("occupantAction %s emitted %d events, want 1. All three values behave "+
					"as Report at v0.4 and the webhook says so; a reserved mode that reported "+
					"nothing would make that disclosure false in the other direction",
					mode, len(got))
			}
			if !strings.Contains(got[0], "amr-1") {
				t.Errorf("event %q does not name the occupant", got[0])
			}
		})
	}
}

// AN OCCUPANT VISIBLE ONLY BY containingZones IS STILL AN OCCUPANT. ADR-0059 made
// containment a SET: a robot inside two overlapping polygons is inside both, and the
// arbitrated currentZone names only one of them. Reading currentZone alone would miss
// the robot the overlap ruling exists for.
func TestOccupantReport_ContainmentSetCountsNotOnlyCurrentZone(t *testing.T) {
	r := robotAt("amr-1", "rid1", "bay") // currentZone arbitrated AWAY from patio
	r.Status.ContainingZones = []string{"bay", "patio"}
	z, _ := newZoneController(t,
		occupantZone("patio", patioPts, "outdoors behind the fence", fleetv1.ZoneOccupantReport),
		r,
	)
	reconcileZone(t, z, "patio")
	got := occupantEvents(drainEvents(t, z))
	if len(got) != 1 || !strings.Contains(got[0], "amr-1") {
		t.Fatalf("events = %v, want one naming amr-1. The robot is inside patio's polygon and "+
			"the nearest-centroid tie-break reported it as being in bay; reading currentZone "+
			"alone misses exactly the robot ADR-0059's containment set exists for", got)
	}
}

// LEAVING AND RE-ENTERING THE DENIED STATE RE-ARMS THE LATCH. A second event is
// correct: it is a second transition, and an operator who lifted a denial and re-applied
// it is entitled to be told who was inside the second time.
func TestOccupantReport_TheLatchReArmsWhenTheDenialIsLifted(t *testing.T) {
	ctx := context.Background()
	z, c := newZoneController(t,
		occupantZone("patio", patioPts, "outdoors behind the fence", fleetv1.ZoneOccupantReport),
		occupantRobot("amr-1", "rid1", "patio"),
	)
	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 1 {
		t.Fatalf("first transition emitted %d, want 1", len(got))
	}

	// Lift the denial.
	var fz fleetv1.FleetZone
	if err := c.Get(ctx, nsName("patio"), &fz); err != nil {
		t.Fatalf("get: %v", err)
	}
	fz.Spec.Restrictions = nil
	fz.Generation++
	if err := c.Update(ctx, &fz); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("lifting the denial emitted %v", got)
	}

	// Re-apply it.
	if err := c.Get(ctx, nsName("patio"), &fz); err != nil {
		t.Fatalf("get: %v", err)
	}
	fz.Spec.Restrictions = &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{
			Mode: fleetv1.ZoneRestrictionDenied, Reason: "closed again",
			OccupantAction: fleetv1.ZoneOccupantReport,
		},
	}
	fz.Generation++
	if err := c.Update(ctx, &fz); err != nil {
		t.Fatalf("update: %v", err)
	}
	reconcileZone(t, z, "patio")
	if got := occupantEvents(drainEvents(t, z)); len(got) != 1 {
		t.Fatalf("%d events on the SECOND transition into denied-with-occupants, want 1: %v. "+
			"The latch records a state, not a fact that has happened once for ever", len(got), got)
	}
}

// nsName is the NamespacedName for a zone in the test namespace.
func nsName(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: actionNS}
}

// getZoneRV reads a zone's resourceVersion. The RA-1 assertions are on this rather than
// on a call count: a status patch writing identical bytes is still an etcd write and
// still a watch event for every controller in the namespace.
func getZoneRV(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var z fleetv1.FleetZone
	if err := c.Get(context.Background(), nsName(name), &z); err != nil {
		t.Fatalf("get zone %q: %v", name, err)
	}
	return z.ResourceVersion
}
