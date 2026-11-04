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
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The cache lags the API server: a stop's trigger written a moment ago is not in it yet.
// An edit that takes a zone or a robot out from under that stop must still be refused, so
// the check reads the zones through the uncached reader, not through the cache.
func TestPendingStopScope_RefusedWhenOnlyTheAPIServerHasSeenTheTrigger(t *testing.T) {
	ctx := context.Background()
	stoppedAnc := xoZone("anc", "")
	stoppedAnc.Annotations = map[string]string{estopTriggeredAnnotation: "anc-stop"}
	// The cache: anc as it was before the trigger.
	cache := xoClient(t, xoAdapter(), xoZone("anc", ""), xoZone("c", "anc"), xoZone("elsewhere", ""))
	// The API server: anc's stop issued, no processed marker yet.
	api := xoClient(t, xoAdapter(), stoppedAnc, xoZone("c", "anc"), xoZone("elsewhere", ""))

	t.Run("re-parent", func(t *testing.T) {
		v := &FleetZoneValidator{Client: cache, APIReader: api}
		_, err := v.ValidateUpdate(ctx, xoZone("c", "anc"), xoZone("c", ""))
		mustRefusePending(t, err)
	})
	t.Run("spec.zone move", func(t *testing.T) {
		g := &RobotAdmissionGate{Client: cache, APIReader: api}
		_, err := g.ValidateUpdate(ctx, xoRobot("rk", "c", "rid-rk"), xoRobot("rk", "elsewhere", "rid-rk"))
		mustRefusePending(t, err)
	})
	t.Run("control: once the stop is applied", func(t *testing.T) {
		applied := stoppedAnc.DeepCopy()
		applied.Annotations[estopProcessedAnnotation] = "anc-stop"
		api := xoClient(t, xoAdapter(), applied, xoZone("c", "anc"), xoZone("elsewhere", ""))
		// Once applied, the re-parent narrows a stop in force, which only its clear releases: refused,
		// naming the clear; admitted once the clear is applied (anc carries neither annotation).
		v := &FleetZoneValidator{Client: cache, APIReader: api}
		if _, err := v.ValidateUpdate(ctx, xoZone("c", "anc"), xoZone("c", "")); err == nil ||
			!strings.Contains(err.Error(), "released only through its clear") {
			t.Errorf("a re-parent out of an applied stop was not refused naming the clear: %v", err)
		}
		cleared := xoClient(t, xoAdapter(), xoZone("anc", ""), xoZone("c", "anc"), xoZone("elsewhere", ""))
		v = &FleetZoneValidator{Client: cache, APIReader: cleared}
		if _, err := v.ValidateUpdate(ctx, xoZone("c", "anc"), xoZone("c", "")); err != nil {
			t.Errorf("a re-parent after the clear was applied was refused: %v", err)
		}
		g := &RobotAdmissionGate{Client: cache, APIReader: api}
		// A robot the applied stop reached records it, and the record travels with it.
		reached := xoRobot("rk", "c", "rid-rk")
		reached.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "anc"}}
		movedReached := xoRobot("rk", "elsewhere", "rid-rk")
		movedReached.Status.EstopHeldBy = reached.Status.EstopHeldBy
		if _, err := g.ValidateUpdate(ctx, reached, movedReached); err != nil {
			t.Errorf("a move of a robot that records the applied stop was refused: %v", err)
		}
		// A robot the applied stop has not reached yet does not leave before it does.
		if _, err := g.ValidateUpdate(ctx, xoRobot("rk", "c", "rid-rk"), xoRobot("rk", "elsewhere", "rid-rk")); err == nil ||
			!strings.Contains(err.Error(), "has not yet reached the robot") {
			t.Errorf("a move out of an applied stop the robot does not record was not refused naming the stop: %v", err)
		}
	})
	t.Run("control: a stop that does not reach the subtree", func(t *testing.T) {
		noProp := stoppedAnc.DeepCopy()
		f := false
		noProp.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: &f}
		api := xoClient(t, xoAdapter(), noProp, xoZone("c", "anc"))
		v := &FleetZoneValidator{Client: cache, APIReader: api}
		if _, err := v.ValidateUpdate(ctx, xoZone("c", "anc"), xoZone("c", "")); err != nil {
			t.Errorf("a re-parent out from under a stop that does not reach children was refused: %v", err)
		}
	})
	t.Run("no uncached reader fails closed", func(t *testing.T) {
		if _, err := pendingStopsOver(ctx, client.Reader(nil), xoNS, "c", true); err == nil {
			t.Errorf("the check ran with no reader")
		}
	})
}

// A propagateToChildren change is refused under a pending stop on the stopped zone and on a zone
// below it, and the message says which: the fan-out reads only the stopped zone's own flag, so only
// a change on that zone alters which robots the stop reaches.
func TestPendingStopScope_PropagateChangeSaysWhetherItChangesTheReach(t *testing.T) {
	ctx := context.Background()
	stoppedAnc := xoZone("anc", "")
	stoppedAnc.Annotations = map[string]string{estopTriggeredAnnotation: "anc-stop"}
	api := xoClient(t, xoAdapter(), stoppedAnc, xoZone("b", "anc"))
	v := &FleetZoneValidator{Client: api, APIReader: api}
	off := func(z *fleetv1.FleetZone) *fleetv1.FleetZone {
		z = z.DeepCopy()
		f := false
		z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: &f}
		return z
	}

	_, err := v.ValidateUpdate(ctx, stoppedAnc, off(stoppedAnc))
	mustRefusePending(t, err)
	if !strings.Contains(err.Error(), `would change which robots the emergency stop of zone "anc" reaches`) {
		t.Errorf("the change on the stopped zone does not say it changes the stop's reach: %v", err)
	}

	_, err = v.ValidateUpdate(ctx, xoZone("b", "anc"), off(xoZone("b", "anc")))
	mustRefusePending(t, err)
	if msg := err.Error(); strings.Contains(msg, "would change which robots") ||
		!strings.Contains(msg, "does not alter which robots it reaches") {
		t.Errorf("the change on a zone below the stopped zone claims to change the stop's reach: %v", err)
	}
}

func mustRefusePending(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("admitted while the API server holds a stop that is issued and not yet applied")
	}
	if !strings.Contains(err.Error(), `zone "anc"`) || !strings.Contains(err.Error(), "Retry once the stop is applied") {
		t.Fatalf("refused without naming the stop and the retry: %v", err)
	}
}
