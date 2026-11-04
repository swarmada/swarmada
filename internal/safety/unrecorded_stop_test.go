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
	"sync"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot in a stop state that records no stop (an upgraded cluster, or a record lost) is sent a
// stop of its own when a SafetyStream opens, and that stop is recorded as a Robot-scope stop whose
// carrier is the robot itself: the CRD requires every Robot-scope stop to name its carrier. A
// second stream sends the same recorded ID again, and records nothing new.
func TestHeldStop_ARobotWithNoRecordGetsOneRecordedUnderItself(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.ContractReported(identity("acme"), "1.4.0")
	setState(t, c, fleetv1.RobotEstopStopped)
	w := &wire{d: d, mu: &sync.Mutex{}, log: &[]string{}}
	d.RegisterStream(identity("acme"), w)
	stops := waitRecorded(t, c, 1)
	r := stored(t, c)
	st := stops[0]
	if st.Scope != fleetv1.StopScopeRobot || st.Carrier == nil || st.Carrier.Kind != fleetv1.StopCarrierRobot ||
		st.Carrier.Name != "amr-1" || st.Carrier.UID != string(r.UID) || st.ClearedAt != nil {
		t.Fatalf("recorded %+v; want a Robot-scope stop whose carrier is amr-1 itself, uncleared", st)
	}
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2)
	for end := time.Now().Add(2 * time.Second); w2.estops() == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(stored(t, c).Status.Stops); n != 1 || w2.estops() != 1 {
		t.Fatalf("second stream: %d records, %d Estops; want the one record, sent again once", n, w2.estops())
	}
	w2.mu.Lock()
	id := w2.out[0].GetEstop().GetEstopId()
	w2.mu.Unlock()
	if id != st.ID {
		t.Fatalf("second stream sent %q; want the recorded ID %q", id, st.ID)
	}
	// The robot's own trigger is not on it, so the stop has ended: the clear names it.
	if s, err := d.ClearEstop(t.Context(), ns, "amr-1", "alice", Ends{}); err != nil || s != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", s, err)
	}
	if cs := w2.clears(); len(cs) != 1 || cs[0].GetEstopId() != st.ID {
		t.Fatalf("clears %v; want one naming %s", cs, st.ID)
	}
}
