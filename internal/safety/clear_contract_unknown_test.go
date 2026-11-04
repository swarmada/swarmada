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
	"context"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The clear on a connection whose contract version this process does not know (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, Version; #fleet-adapter-protocol-package-and-versioning).
// The control plane sends a 1.3.x connection no EstopClear, and a version it cannot read is never
// an implicit pass. A new leader knows no adapter's version until the adapter's next ControlStream
// reports it (clearRigAt with no version: a new leader's memory). Until then an
// EstopClear, a release, is not sent: the entry is kept with its clearedAt and holds nothing. The
// report decides: 1.4.0 sends the EstopClear the entry awaits, below 1.4.0 removes the entry.

// waitClears waits up to two seconds for w to carry n EstopClears, and returns how many it carries.
func waitClears(w *wire, n int) int {
	for end := time.Now().Add(2 * time.Second); len(w.clears()) < n && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	return len(w.clears())
}

// clearUnknown is the clear on clearRig's robot with no contract version reported: Resuming is
// written, the entry is kept with its clearedAt, and no EstopClear is sent.
func clearUnknown(t *testing.T) (*Dispatcher, *hookClient, *wire, string) {
	t.Helper()
	d, h, w, id := clearRigAt(t, "")
	d.APIReader = h.Client
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	r := stored(t, h.Client)
	if len(r.Status.Stops) != 1 || r.Status.Stops[0].ID != id || r.Status.Stops[0].ClearedAt == nil {
		t.Fatalf("stops %+v; want the entry kept with its clearedAt", r.Status.Stops)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(w.clears()); n != 0 {
		t.Fatalf("%d EstopClears to an adapter whose contract version is not known; want none", n)
	}
	return d, h, w, id
}

func TestClear_AnUnknownContractVersionSendsNoEstopClearAndKeepsTheEntry(t *testing.T) {
	clearUnknown(t)
}

// A SafetyStream that opens while the version is still unknown is sent no EstopClear either.
func TestClear_ANewSafetyStreamOfAnUnknownContractIsSentNoEstopClear(t *testing.T) {
	d, h, w, _ := clearUnknown(t)
	w2 := &wire{d: d, mu: w.mu, log: w.log}
	d.RegisterStream(identity("acme"), w2)
	time.Sleep(200 * time.Millisecond)
	if n := len(w2.clears()); n != 0 || len(stored(t, h.Client).Status.Stops) != 1 {
		t.Fatalf("new stream: %d EstopClears, %d entries; want none and the entry kept", n, len(stored(t, h.Client).Status.Stops))
	}
}

// The adapter's report decides what the kept entry gets: 1.4.0 sends its EstopClear on the current
// SafetyStream, once, naming the stop; 1.3.0 removes it and sends none.
func TestClear_TheReportAfterAnUnknownContractDecides(t *testing.T) {
	t.Run("1.4.0-sends-the-clear", func(t *testing.T) {
		d, h, w, id := clearUnknown(t)
		d.ContractReported(identity("acme"), "1.4.0")
		if n := waitClears(w, 1); n != 1 || w.clears()[0].GetEstopId() != id {
			t.Fatalf("%d EstopClears after a 1.4.0 report; want the one the entry awaits (%s)", n, id)
		}
		if n := len(stored(t, h.Client).Status.Stops); n != 1 {
			t.Fatalf("%d entries; want the entry kept until its EstopClearAck", n)
		}
	})
	t.Run("1.3.0-removes-the-entry", func(t *testing.T) {
		d, h, w, _ := clearUnknown(t)
		d.ContractReported(identity("acme"), "1.3.0")
		waitRecorded(t, h.Client, 0)
		time.Sleep(50 * time.Millisecond)
		if n := len(w.clears()); n != 0 {
			t.Fatalf("%d EstopClears to a 1.3.0 adapter; want none", n)
		}
	})
}

// A version this control plane does not support, or cannot read, is not a pass either: the clear
// sends nothing and keeps the entry.
func TestClear_AnUnsupportedContractVersionSendsNoEstopClear(t *testing.T) {
	for _, v := range []string{"2.0.0", "1.4", "not-a-version"} {
		t.Run(v, func(t *testing.T) {
			d, h, w, _ := clearRigAt(t, v)
			if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
				t.Fatalf("ClearEstop = %s, %v", st, err)
			}
			time.Sleep(50 * time.Millisecond)
			if n, e := len(w.clears()), len(stored(t, h.Client).Status.Stops); n != 0 || e != 1 {
				t.Fatalf("contract %q: %d EstopClears, %d entries; want none and the entry kept", v, n, e)
			}
		})
	}
}
