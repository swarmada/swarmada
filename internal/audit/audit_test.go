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

package audit

import (
	"fmt"
	"sync"
	"testing"
)

func recordN(t *testing.T, l *Log, ns string, n int) *MemorySink {
	t.Helper()
	sink := l.sink.(*MemorySink)
	for i := 0; i < n; i++ {
		if _, err := l.Record(Entry{
			Namespace: ns, EventType: EventEstopTriggered, Action: "estop-trigger",
			Outcome: OutcomeAllowed, Resource: Resource{Kind: "Robot", Namespace: ns, Name: fmt.Sprintf("amr-%d", i)},
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	return sink
}

// A clean chain verifies: sequence 1..N, genesis-seeded, no mismatches.
func TestVerify_CleanChainPasses(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	sink := recordN(t, l, "warehouse-a", 5)
	res := Verify(sink.ForNamespace("warehouse-a"))
	if !res.OK || res.Entries != 5 || res.SequenceGaps != 0 || res.HashMismatches != 0 || !res.GenesisOK {
		t.Fatalf("clean chain: %+v, want OK/5/0/0/genesis", res)
	}
}

// CARDINAL TAMPER TEST: modifying any field of a sealed entry breaks its
// chain_hash and is detected.
func TestVerify_ModifiedEntryDetected(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	entries := recordN(t, l, "warehouse-a", 5).ForNamespace("warehouse-a")

	// An attacker rewrites the reason/target of entry 2 in place.
	entries[2].Resource.Name = "amr-tampered"
	entries[2].Detail = map[string]string{"injected": "true"}

	res := Verify(entries)
	if res.OK || res.HashMismatches == 0 {
		t.Fatalf("tampered entry not detected: %+v", res)
	}
}

// Deleting an entry breaks sequence continuity (and the successor's prev-hash).
func TestVerify_DeletionDetected(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	entries := recordN(t, l, "warehouse-a", 5).ForNamespace("warehouse-a")

	// Remove entry index 2 (sequence 3).
	tampered := append(append([]Entry{}, entries[:2]...), entries[3:]...)
	res := Verify(tampered)
	if res.OK || (res.SequenceGaps == 0 && res.HashMismatches == 0) {
		t.Fatalf("deletion not detected: %+v", res)
	}
}

// Reordering two entries breaks the chain.
func TestVerify_ReorderDetected(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	entries := recordN(t, l, "warehouse-a", 5).ForNamespace("warehouse-a")
	entries[1], entries[3] = entries[3], entries[1]
	res := Verify(entries)
	if res.OK {
		t.Fatalf("reorder not detected: %+v", res)
	}
}

// A Denied action is recorded (never dropped, §9.5.4) and part of the chain.
func TestRecord_DeniedNeverDropped(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	sealed, err := l.Record(Entry{
		Namespace: "warehouse-a", EventType: EventRobotAuthzDenied, Action: "telemetry",
		Outcome: OutcomeDenied, Actor: Actor{Type: ActorRobot, Identity: "acme.warehouse-a"},
		Resource: Resource{Kind: "Robot", Namespace: "warehouse-a", Name: "amr-forged"},
	})
	if err != nil {
		t.Fatalf("record denied: %v", err)
	}
	if sealed.Outcome != OutcomeDenied || sealed.SequenceNumber != 1 || sealed.ChainHash == "" {
		t.Fatalf("denied entry not sealed: %+v", sealed)
	}
	if got := l.sink.(*MemorySink).ForNamespace("warehouse-a"); len(got) != 1 {
		t.Fatalf("denied entry not persisted: %d", len(got))
	}
}

// Chains are independent per namespace (each seeds from genesis).
func TestRecord_PerNamespaceChains(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	a, _ := l.Record(Entry{Namespace: "a", EventType: EventEstopTriggered, Outcome: OutcomeAllowed})
	b, _ := l.Record(Entry{Namespace: "b", EventType: EventEstopTriggered, Outcome: OutcomeAllowed})
	if a.SequenceNumber != 1 || b.SequenceNumber != 1 {
		t.Fatalf("per-namespace sequences not independent: a=%d b=%d", a.SequenceNumber, b.SequenceNumber)
	}
	sink := l.sink.(*MemorySink)
	if !Verify(sink.ForNamespace("a")).OK || !Verify(sink.ForNamespace("b")).OK {
		t.Fatal("per-namespace chains must each verify")
	}
}

// A sink failure rolls the chain back so it never advances over a dropped write.
func TestRecord_SinkFailureRollsBack(t *testing.T) {
	fs := &failingSink{fail: true}
	l := New(fs, "v0.1.0")
	if _, err := l.Record(Entry{Namespace: "a", EventType: EventEstopTriggered}); err == nil {
		t.Fatal("expected sink error")
	}
	// Next successful record must be sequence 1 (the failed one did not advance).
	fs.fail = false
	sealed, err := l.Record(Entry{Namespace: "a", EventType: EventEstopTriggered})
	if err != nil || sealed.SequenceNumber != 1 {
		t.Fatalf("chain advanced over a failed write: seq=%d err=%v", sealed.SequenceNumber, err)
	}
}

type failingSink struct {
	fail bool
	mem  MemorySink
}

func (f *failingSink) Append(e Entry) error {
	if f.fail {
		return fmt.Errorf("sink unavailable")
	}
	return f.mem.Append(e)
}

// Concurrent records across namespaces are race-free and each chain verifies.
func TestRecord_ConcurrentRaceFree(t *testing.T) {
	l := New(&MemorySink{}, "v0.1.0")
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		ns := fmt.Sprintf("ns-%d", n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = l.Record(Entry{Namespace: ns, EventType: EventEstopTriggered, Outcome: OutcomeAllowed})
			}
		}()
	}
	wg.Wait()
	sink := l.sink.(*MemorySink)
	for n := 0; n < 4; n++ {
		ns := fmt.Sprintf("ns-%d", n)
		if res := Verify(sink.ForNamespace(ns)); !res.OK || res.Entries != 20 {
			t.Fatalf("namespace %s chain invalid: %+v", ns, res)
		}
	}
}
