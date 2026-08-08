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
	"path/filepath"
	"sync"
	"testing"
)

func nsEntries(entries []Entry, ns string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Namespace == ns {
			out = append(out, e)
		}
	}
	return out
}

// Entries recorded through a FileSink are durably persisted and read back with an
// intact hash chain per namespace.
func TestFileSink_RoundTripVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	sink, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	log := New(sink, "v0.1.0")

	// Interleave two namespaces (two independent chains).
	for i := 0; i < 3; i++ {
		if _, err := log.Record(Entry{Namespace: "warehouse-a", EventType: EventEstopTriggered, Action: "estop-trigger",
			Actor: Actor{Type: ActorUser, Identity: "op"}, Resource: Resource{Kind: "FleetZone", Namespace: "warehouse-a", Name: "z"}, Outcome: OutcomeAllowed}); err != nil {
			t.Fatalf("record a: %v", err)
		}
		if _, err := log.Record(Entry{Namespace: "warehouse-b", EventType: EventModelRolloutCreated, Action: "create",
			Actor: Actor{Type: ActorServiceAccount, Identity: "ctrl"}, Resource: Resource{Kind: "ModelRollout", Namespace: "warehouse-b", Name: "r"}, Outcome: OutcomeAllowed}); err != nil {
			t.Fatalf("record b: %v", err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("read %d entries, want 6", len(got))
	}
	for _, ns := range []string{"warehouse-a", "warehouse-b"} {
		if res := Verify(nsEntries(got, ns)); !res.OK {
			t.Errorf("namespace %q chain failed Verify after round-trip: %+v", ns, res)
		}
	}
}

// A crash-simulating reopen keeps prior entries and appends more (durability).
func TestFileSink_AppendsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")

	s1, _ := NewFileSink(path)
	l1 := New(s1, "v")
	_, _ = l1.Record(Entry{Namespace: "n", EventType: EventEstopTriggered, Action: "a"})
	_ = s1.Close()

	// Reopen (new process would rebuild chain state from the file; here a fresh Log
	// starts a new chain, which is acceptable for the durability check).
	s2, _ := NewFileSink(path)
	l2 := New(s2, "v")
	_, _ = l2.Record(Entry{Namespace: "n", EventType: EventEstopCleared, Action: "a"})
	_ = s2.Close()

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries after reopen, want 2 (prior entry survived)", len(got))
	}
}

// Concurrent producers append safely (the sink + Log.Record serialise writes).
func TestFileSink_ConcurrentAppendsAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	sink, _ := NewFileSink(path)
	log := New(sink, "v")

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = log.Record(Entry{Namespace: "n", EventType: EventEstopTriggered, Action: fmt.Sprintf("a%d", i)})
		}(i)
	}
	wg.Wait()
	_ = sink.Close()

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != n {
		t.Fatalf("read %d entries, want %d (no lost/corrupt concurrent appends)", len(got), n)
	}
	if res := Verify(got); !res.OK {
		t.Errorf("concurrent chain failed Verify: %+v", res)
	}
}

// A write after Close fails closed — Log.Record then does NOT advance the chain.
func TestFileSink_AppendAfterCloseFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	sink, _ := NewFileSink(path)
	_ = sink.Close()

	if err := sink.Append(Entry{Namespace: "n", EventType: EventEstopTriggered}); err == nil {
		t.Fatal("Append to a closed FileSink must return an error (fail-closed durability)")
	}
}
