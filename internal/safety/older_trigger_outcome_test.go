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

package safety

import (
	"strings"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Two triggers for one robot are pending at once; the newer is confirmed STOPPED and the
// older then reaches the end of its delivery window. The older trigger's Failed does not
// overwrite the newer confirmed Stopped: a confirmed stop is never discarded
// (RFC-0001 #safety-estop-state-machine). The older trigger still returns Failed, which is
// what its caller audits, and the dispatcher records a Warning Event for it.
func TestEstopOutcome_AnOlderTriggersFailureDoesNotOverwriteANewerConfirmedStop(t *testing.T) {
	// From Normal the newer trigger's Stopped is a write; from a robot already Stopped
	// (an earlier episode never cleared) it is not, and must stand all the same.
	for _, from := range []fleetv1.RobotEstopState{fleetv1.RobotEstopNormal, fleetv1.RobotEstopStopped} {
		t.Run("from "+string(from), func(t *testing.T) {
			d, c, rec := newDispatcher(t, "acme")
			setState(t, c, from)
			d.deliveryTimeout = 400 * time.Millisecond
			s := newRecordingSender()
			d.RegisterStream(identity("acme"), s)
			if from != fleetv1.RobotEstopNormal {
				heldStopResent(t, s)
			}

			older := triggerAsync(d, "a")
			nextEstop(t, s, "the older trigger")
			newer := triggerAsync(d, "b")
			b := nextEstop(t, s, "the newer trigger")
			if err := d.RouteAck(identity("acme"), s, "amr-1",
				ackState(b.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
				t.Fatal(err)
			}
			if o := outcome(t, newer); !o.res.Confirmed {
				t.Fatalf("the newer trigger: %+v; want confirmed", o.res)
			}

			o := outcome(t, older) // silent: resolves at the end of its delivery window
			if o.res.State != fleetv1.RobotEstopFailed || o.res.Confirmed {
				t.Fatalf("the older trigger returned %+v; its own outcome is still Failed", o.res)
			}
			if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
				t.Fatalf("estopState = %s; an older trigger's timeout overwrote a newer confirmed Stopped", got)
			}
			select {
			case e := <-rec.Events:
				if !strings.Contains(e, "Warning EstopOutcomeSuperseded") {
					t.Fatalf("event = %q; want an EstopOutcomeSuperseded Warning", e)
				}
			default:
				t.Fatal("the older trigger's timeout left no Event")
			}
		})
	}
}

// The older trigger's intermediate STOPPING does not overwrite the newer confirmed Stopped
// either, nor does its confirm-window timeout after it.
func TestEstopOutcome_AnOlderTriggersStoppingDoesNotOverwriteANewerConfirmedStop(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 2 * time.Second
	d.confirmTimeout = 200 * time.Millisecond
	s := newRecordingSender()
	d.RegisterStream(identity("acme"), s)

	older := triggerAsync(d, "a")
	a := nextEstop(t, s, "the older trigger")
	newer := triggerAsync(d, "b")
	b := nextEstop(t, s, "the newer trigger")
	if err := d.RouteAck(identity("acme"), s, "amr-1",
		ackState(b.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	outcome(t, newer)
	if err := d.RouteAck(identity("acme"), s, "amr-1",
		ackState(a.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPING)); err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, older); o.res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("the older trigger returned %+v; want Failed at its confirm window", o.res)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s; want the newer confirmed Stopped", got)
	}
}

// The newest outcome is the truth: a NEWER trigger's Failed still overwrites an older
// trigger's confirmed Stopped, whether the older one resolved before the newer was
// issued or while both were pending.
func TestEstopOutcome_ANewerTriggersFailureOverwritesAnOlderConfirmedStop(t *testing.T) {
	t.Run("older resolved before the newer was issued", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 200 * time.Millisecond
		s := newRecordingSender()
		d.RegisterStream(identity("acme"), s)

		older := triggerAsync(d, "a")
		a := nextEstop(t, s, "the older trigger")
		if err := d.RouteAck(identity("acme"), s, "amr-1",
			ackState(a.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
			t.Fatal(err)
		}
		outcome(t, older)
		if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
			t.Fatalf("estopState = %s after the older trigger was confirmed", got)
		}

		newer := triggerAsync(d, "b")
		nextEstop(t, s, "the newer trigger")
		if o := outcome(t, newer); o.res.State != fleetv1.RobotEstopFailed {
			t.Fatalf("the newer trigger returned %+v; want Failed", o.res)
		}
		if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
			t.Fatalf("estopState = %s; the newer trigger's Failed must overwrite the older Stopped", got)
		}
	})

	t.Run("both pending, the older confirmed first", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 400 * time.Millisecond
		s := newRecordingSender()
		d.RegisterStream(identity("acme"), s)

		older := triggerAsync(d, "a")
		a := nextEstop(t, s, "the older trigger")
		newer := triggerAsync(d, "b")
		nextEstop(t, s, "the newer trigger")
		if err := d.RouteAck(identity("acme"), s, "amr-1",
			ackState(a.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
			t.Fatal(err)
		}
		outcome(t, older)
		if o := outcome(t, newer); o.res.State != fleetv1.RobotEstopFailed {
			t.Fatalf("the newer trigger returned %+v; want Failed", o.res)
		}
		if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
			t.Fatalf("estopState = %s; the newer trigger's Failed must overwrite the older Stopped", got)
		}
	})
}
