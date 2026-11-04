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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Rows of the estop hold invariant that read the hold record itself
// (Robot.status.estopHeldBy) and the resume claim.
func recordRows() []invariantRow {
	return []invariantRow{
		// A robot stopped before the record existed gets one on the first reconcile of the
		// stop in force. It has no record of the zone's stop either, so it is sent that stop
		// once, as its own stop, and the stop is recorded (RFC-0001 #safety-robot-stop-table, S1);
		// a later pass over the zone sends nothing more.
		{name: "upgrade-a-stopped-robot-gets-its-record", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			m := r.w.as(t, controllerUser)
			z := r.zoneObj("z")
			z.Annotations = map[string]string{estopTriggeredAnnotation: "old", estopProcessedAnnotation: "old"}
			r.must("creating the stopped zone as the control plane", m.Create(context.Background(), z))
			r.robot("r1", "z")
			holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, true)
			r.start()
			if ok, _ := r.waitFor("r1 records the zone's stop", 15*time.Second, func() (bool, string) {
				h := r.get("r1").Status.EstopHeldBy
				return len(h) == 1 && h[0].Kind == fleetv1.EstopHoldFleetZone && h[0].Name == "z", holdsOf(h)
			}); !ok {
				t.Errorf("a robot stopped before the record existed has no record after the first reconcile")
			}
			// Two stops: the held stop re-sent when the adapter's SafetyStream opens on the new
			// manager, recorded as a stop of the robot's own (it had no record), and the zone's
			// stop, which the robot had no record of either.
			if ok, last := r.waitFor("r1 records the zone's stop in status.stops", 15*time.Second, func() (bool, string) {
				zone, own := 0, 0
				for _, st := range r.get("r1").Status.Stops {
					switch {
					case st.Carrier != nil && st.Carrier.Kind == fleetv1.StopCarrierFleetZone && st.Carrier.Name == "z":
						zone++
					case st.Carrier != nil && st.Carrier.Kind == fleetv1.StopCarrierRobot && st.Carrier.Name == "r1":
						own++
					}
				}
				st := r.get("r1").Status.Stops
				return zone == 1 && own == 1 && len(st) == 2, fmt.Sprint(st)
			}); !ok {
				t.Errorf("the zone's stop sent to r1 is not recorded once in status.stops: %s", last)
			}
			n := r.settledSends("r1")
			if n != 2 {
				t.Errorf("an already stopped robot with no record of the zone's stop was sent %d stops, want 2 "+
					"(the re-send on the new stream and the zone's stop)", n)
			}
			// A later pass over the zone: the record has landed, so nothing more is sent.
			r.setAnnotation(r.zoneObj("z"), "swarmada.io/test-pass", strp("1"))
			if m := r.settledSends("r1"); m != n {
				t.Errorf("a later pass over the zone sent r1 %d more stops, want none", m-n)
			}
		}},
		// A recorded stop whose carrier is gone holds until an admin's robot-scope clear.
		{name: "orphaned-record-released-by-a-robot-scope-clear", run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("y", "", nil)
			r.robot("r1", "z")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true) // the move comes after z's stop is applied
			r.setSpecZone("r1", "y")
			// z carries its stop, so it is not deleted (a stop is released only through its clear).
			refusedWhileInForce(t, "the admin's delete of z", `"z"`, r.w.admin.Delete(context.Background(), r.zoneObj("z")))
			r.assertHeld("r1", true)
			// A record whose carrier is gone still arises where that refusal does not hold (an install
			// without the webhooks, a namespace being deleted); the control plane's own write of one
			// stands in for it on a second robot.
			r.robot("r2", "y")
			m := r.w.as(t, controllerUser)
			rb := r.robotObj("r2")
			r.must("a stop recorded from a zone that no longer exists", r.update(m, rb, true, func() {
				rb.Status.EstopState = fleetv1.RobotEstopStopped
				rb.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "gone",
					UID: "gone-uid", Since: metav1.Now()}}
			}))
			msg := r.resume("r2", "orphan-1")
			if !strings.Contains(msg, "no longer exists") || !strings.Contains(msg, "kubectl annotate robot r2") {
				t.Errorf("the refusal does not name the missing stop and its release: %q", msg)
			}
			r.ownRelease("r2")
			if ok, _ := r.waitFor("r2 Resuming with no record", 10*time.Second, func() (bool, string) {
				got := r.get("r2")
				return got.Status.EstopState == fleetv1.RobotEstopResuming && len(got.Status.EstopHeldBy) == 0,
					string(got.Status.EstopState) + " " + holdsOf(got.Status.EstopHeldBy)
			}); !ok {
				t.Errorf("the robot-scope clear did not release the orphaned record")
			}
			t.Logf("RESULT orphan resume: %s", r.resume("r2", "orphan-2"))
			if s := r.get("r2").Status.EstopState; s != fleetv1.RobotEstopNormal {
				t.Errorf("after the orphaned record's release and a resume the robot is %q", s)
			}
		}},
		// A robot that records a stop in force but reads Normal (its stop write never
		// landed) is stopped when the manager starts: the Estopper is called for it.
		{name: "normal-robot-with-a-record-is-stopped-on-start", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			m := r.w.as(t, controllerUser)
			z := r.zoneObj("z")
			z.Annotations = map[string]string{estopTriggeredAnnotation: "zs", estopProcessedAnnotation: "zs"}
			r.must("creating the stopped zone as the control plane", m.Create(context.Background(), z))
			r.must("reading z", r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(z), z))
			r.zone("out", "", nil)
			r.robot("r1", "out")
			rb := r.robotObj("r1")
			r.must("writing the record as the control plane", r.update(m, rb, true, func() {
				rb.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z",
					UID: string(z.UID), Since: metav1.Now()}}
			}))
			r.start()
			r.assertHeld("r1", true)
			if n := r.sendsTo("r1"); n < 1 {
				t.Errorf("the robot that records the stop was not sent it (sends=%d)", n)
			}
		}},
		// A resume request claimed and never answered is reported, and not evaluated.
		{name: "claimed-resume-never-answered-is-reported", run: func(t *testing.T, r *rig) {
			r.robot("r1", markerRobotZone)
			m := r.w.as(t, controllerUser)
			rb := r.robotObj("r1")
			r.must("Resuming", r.update(m, rb, true, func() { rb.Status.EstopState = fleetv1.RobotEstopResuming }))
			r.must("a claim left by a stopped control plane", r.update(m, rb, false, func() {
				if rb.Annotations == nil {
					rb.Annotations = map[string]string{}
				}
				rb.Annotations[fleetv1.EstopResumeAnnotation] = "lost"
				rb.Annotations[fleetv1.EstopResumeProcessedAnnotation] = "lost"
				rb.Annotations[fleetv1.EstopResumeClaimedAnnotation] = "lost"
			}))
			if ok, _ := r.waitFor("EstopResumeUnanswered on r1", 10*time.Second, func() (bool, string) {
				var evs corev1.EventList
				_ = r.w.admin.List(context.Background(), &evs, client.InNamespace(r.w.ns))
				for _, e := range evs.Items {
					if e.InvolvedObject.Name == "r1" && e.Reason == "EstopResumeUnanswered" {
						return true, e.Message
					}
				}
				return false, ""
			}); !ok {
				t.Errorf("a claimed request left unanswered was not reported")
			}
			if ok, _ := r.waitFor("r1's claim closed", 10*time.Second, func() (bool, string) {
				_, still := r.get("r1").Annotations[fleetv1.EstopResumeClaimedAnnotation]
				return !still, ""
			}); !ok {
				t.Errorf("the claim was not closed")
			}
			if r.res.count() != 0 {
				t.Errorf("the unanswered request was evaluated after all (%d)", r.res.count())
			}
		}},
	}
}

func holdsOf(h []fleetv1.EstopHold) string {
	var s []string
	for _, e := range h {
		s = append(s, string(e.Kind)+"/"+e.Name)
	}
	return "[" + strings.Join(s, ",") + "]"
}
