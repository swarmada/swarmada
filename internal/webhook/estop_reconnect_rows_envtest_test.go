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
	"fmt"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
)

// A robot whose adapter reconnects while its estop is settled is sent the stop again on
// the new SafetyStream, Failed included, once per stream that opens (RFC-0001
// #safety-emergency-stop-protocol). A robot in Normal is sent nothing, nor is one in
// Resuming, whose stop an operator has cleared. Each row counts the
// estops the scripted stream carries for the robot, which is what the adapter receives.

// reconnect opens a new SafetyStream for the rig's adapter. With closeFirst the old stream
// closes before the new one opens, as when the adapter restarts; without it the new stream
// replaces the old one, as when the adapter re-dials before the old connection is gone.
func (r *rig) reconnect(closeFirst bool) {
	if closeFirst {
		r.dereg()
	}
	id := controlstream.TLSIdentity{AdapterName: markerRobotAdapter, Namespace: r.w.ns, Verified: true}
	r.dereg = r.d.RegisterStream(id, &scriptSender{r: r, d: r.d, id: id})
}

// sendsSettleAt requires the estops sent to robot to reach want, and to stay there.
func (r *rig) sendsSettleAt(robot string, want int, what string) {
	r.t.Helper()
	ok, last := r.waitFor(fmt.Sprintf("%s: %d estops sent to %s", what, want, robot), 5*time.Second,
		func() (bool, string) { n := r.sendsTo(robot); return n == want, fmt.Sprint(n) })
	if !ok {
		r.t.Fatalf("%s: %s estops sent to %s, want %d", what, last, robot, want)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := r.sendsTo(robot); n != want {
		r.t.Fatalf("%s: %d estops sent to %s after it settled at %d; a re-send repeated", what, n, robot, want)
	}
}

// settledSends waits until the estops sent to robot stop changing, and returns how many.
// A stop's own trigger is the reconciler's to send, and may be sent more than once; a
// row counts only what a reconnect adds to the number it settled at.
func (r *rig) settledSends(robot string) int {
	r.t.Helper()
	n := r.sendsTo(robot)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
		time.Sleep(1500 * time.Millisecond)
		m := r.sendsTo(robot)
		if m == n {
			r.t.Logf("RESULT estops sent to %s settled at %d", robot, n)
			return n
		}
		n = m
	}
	r.t.Fatalf("the estops sent to %s never settled (%d)", robot, n)
	return 0
}

// inForceStops is the number of stops recorded on robot with no clearedAt: a SafetyStream that
// opens sends each of them again, under its own ID (Sending a stop again). A robot's own trigger
// may be sent, and so recorded, more than once.
func (r *rig) inForceStops(robot string) int {
	r.t.Helper()
	n := 0
	for _, st := range r.get(robot).Status.Stops {
		if st.ClearedAt == nil {
			n++
		}
	}
	if n == 0 {
		r.t.Fatalf("%s records no stop in force", robot)
	}
	return n
}

// waitState requires robot to read state.
func (r *rig) waitState(robot string, state fleetv1.RobotEstopState) {
	r.t.Helper()
	if ok, last := r.waitFor(fmt.Sprintf("%s reads %s", robot, state), 15*time.Second, func() (bool, string) {
		s := r.get(robot).Status.EstopState
		return s == state, string(s)
	}); !ok {
		r.t.Fatalf("%s reads %s, want %s", robot, last, state)
	}
}

func reconnectRows() []invariantRow {
	var rows []invariantRow

	// Confirmed Stopped; the adapter restarts, then re-dials over a live connection.
	rows = append(rows, invariantRow{name: "reconnect/Stopped-resent-once-per-stream", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.hold("a-held", "r1", false)
		r.trigger(r.robotObj("r1"), "own")
		r.waitState("r1", fleetv1.RobotEstopStopped)
		n := r.settledSends("r1")
		k := r.inForceStops("r1")
		r.reconnect(true)
		r.sendsSettleAt("r1", n+k, "the adapter restarted")
		r.reconnect(false)
		r.sendsSettleAt("r1", n+2*k, "the adapter re-dialled")
		r.waitState("r1", fleetv1.RobotEstopStopped)
		r.assertHeld("r1", true, "a-held")
	}})

	// Failed because no stream was up when the stop was issued: the robot never got it.
	rows = append(rows, invariantRow{name: "reconnect/Failed-no-stream-resent", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.hold("a-held", "r1", false)
		r.dereg()
		r.trigger(r.robotObj("r1"), "own")
		r.waitState("r1", fleetv1.RobotEstopFailed)
		n := r.settledSends("r1")
		k := r.inForceStops("r1")
		r.reconnect(false)
		r.sendsSettleAt("r1", n+k, "the adapter reconnected")
		r.waitState("r1", fleetv1.RobotEstopFailed)
		r.assertHeld("r1", true, "a-held")
	}})

	// Failed because the adapter never answered; the re-send goes unanswered too, and is
	// not sent again on the same stream. A stream that replaces it while the re-send is
	// still awaited sends the stop once more, itself, and does not also move the old one.
	rows = append(rows, invariantRow{name: "reconnect/Failed-unanswered-resent-once", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.setMode("r1", ackNone)
		r.trigger(r.robotObj("r1"), "own")
		r.waitState("r1", fleetv1.RobotEstopFailed)
		n := r.settledSends("r1")
		k := r.inForceStops("r1")
		r.reconnect(true)
		r.sendsSettleAt("r1", n+k, "the adapter restarted")
		r.reconnect(false)
		r.sendsSettleAt("r1", n+2*k, "the adapter re-dialled with the re-send unanswered")
		time.Sleep(12 * time.Second) // past the re-sends' delivery and confirmation windows
		if got := r.sendsTo("r1"); got != n+2*k {
			t.Fatalf("%d estops sent after the re-sends went unanswered; want %d", got, n+2*k)
		}
		r.waitState("r1", fleetv1.RobotEstopFailed)
	}})

	// Normal: nothing to re-send.
	rows = append(rows, invariantRow{name: "reconnect/Normal-nothing-sent", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.reconnect(true)
		r.sendsSettleAt("r1", 0, "the adapter restarted")
		r.reconnect(false)
		r.sendsSettleAt("r1", 0, "the adapter re-dialled")
		if s := r.get("r1").Status.EstopState; s != "" && s != fleetv1.RobotEstopNormal {
			t.Fatalf("r1 reads %s; want Normal", s)
		}
	}})

	// Resuming: an operator cleared the stop, and a cleared stop is not sent again.
	rows = append(rows, invariantRow{name: "reconnect/Resuming-nothing-sent", run: func(t *testing.T, r *rig) {
		r.robot("r1", markerRobotZone)
		r.trigger(r.robotObj("r1"), "own")
		r.waitState("r1", fleetv1.RobotEstopStopped)
		r.clearStop(r.robotObj("r1"))
		r.waitState("r1", fleetv1.RobotEstopResuming)
		n := r.settledSends("r1")
		r.reconnect(true)
		r.sendsSettleAt("r1", n, "the adapter restarted")
		r.reconnect(false)
		r.sendsSettleAt("r1", n, "the adapter re-dialled")
		r.waitState("r1", fleetv1.RobotEstopResuming)
	}})

	return rows
}
