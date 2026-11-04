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
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A superseded ControlStream is reported to the operator as one Warning Event on the
// FleetAdapter, and costs no status write beyond the new stream's own connect.
func TestControlStream_SupersessionEmitsOneWarningEvent(t *testing.T) {
	w := newSupWorld(t, 60)
	rec := record.NewFakeRecorder(8)
	w.r.Recorder = rec

	old := w.connect(t, supAdapter, nil)
	w.waitPhase(t, fleetv1.FleetAdapterPhaseConnected)
	select {
	case ev := <-rec.Events:
		t.Fatalf("an Event before any supersession: %s", ev)
	default:
	}

	w.connect(t, supAdapter, nil)
	select {
	case ev := <-rec.Events:
		if !strings.HasPrefix(ev, "Warning ControlStreamSuperseded ") {
			t.Fatalf("event = %q, want a Warning ControlStreamSuperseded", ev)
		}
		t.Logf("event: %s", ev)
	case <-time.After(3 * time.Second):
		t.Fatal("no Event for the superseded stream")
	}
	if !old.closed(3 * time.Second) {
		t.Fatal("superseded stream still open")
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-rec.Events:
		t.Fatalf("a second Event for one supersession: %s", ev)
	default:
	}
}
