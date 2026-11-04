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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// SentUnrecorded is the robots a carrier's stop has reached before its record lands: this
// process sent it, has not recorded it, and no clear has ended it (RFC-0001 #safety-estop-scopes).
// It names the carrier's robots in the namespace only, by kind, name and UID.
func TestSentUnrecorded_TheCarriersStopsInFlightThatNoClearEnded(t *testing.T) {
	d := New(nil, nil)
	zone := Carrier{Kind: fleetv1.StopCarrierFleetZone, Name: "z", UID: "z-uid"}
	track := func(namespace, robot string, scope metrics.EstopScope, c Carrier) *stopRecord {
		rb := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: robot, Namespace: namespace, UID: types.UID("uid-" + robot)}}
		r := d.newStopRecord(rb, NewStopID(), scope, c, time.Now())
		d.trackInflight(robotEstopKey(namespace, robot), r)
		return r
	}
	track(ns, "sent", metrics.ScopeZone, zone)
	track(ns, "no-uid", metrics.ScopeZone, Carrier{Kind: zone.Kind, Name: "z"})
	track("other-ns", "elsewhere", metrics.ScopeZone, zone)
	track(ns, "other-zone", metrics.ScopeZone, Carrier{Kind: zone.Kind, Name: "y", UID: "y-uid"})
	track(ns, "recreated-zone", metrics.ScopeZone, Carrier{Kind: zone.Kind, Name: "z", UID: "old-uid"})
	track(ns, "namespace-stop", metrics.ScopeNamespace, Carrier{Kind: fleetv1.StopCarrierSwarmadaConfig, Name: "z", UID: "z-uid"})
	track(ns, "own-stop", metrics.ScopeRobot, Carrier{})
	cleared := track(ns, "cleared", metrics.ScopeZone, zone)
	at := time.Now()
	d.mu.Lock()
	cleared.clearedAt = &at
	d.mu.Unlock()

	got := d.SentUnrecorded(ns, zone)
	want := map[string]bool{"sent": true, "no-uid": true}
	for name := range want {
		if !got[name] {
			t.Errorf("%s, sent z's stop and not recorded, is not in %v", name, got)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s is in SentUnrecorded(z) = %v: it is not an unrecorded, uncleared stop of z in %s", name, got, ns)
		}
	}
}
