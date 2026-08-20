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
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Chain continuity across a process restart (Round-4 D6).
//
// Before Resume(), a restarted control plane built a fresh Log whose per-namespace state
// began at sequence 0 with the genesis hash, and appended that second chain into the SAME
// file. The result verified as TAMPERED — a sequence gap and a hash mismatch at the seam —
// on a log that had in fact never been touched. The pre-existing
// TestFileSink_AppendsAcrossReopen documents that old behaviour explicitly ("a fresh Log
// starts a new chain, which is acceptable for the durability check"); it checks durability,
// not continuity, so nothing covered the seam.

func resumeEntry(ns, evt string) Entry {
	return Entry{
		Namespace: ns, EventType: evt, Action: "a",
		Actor:    Actor{Type: ActorUser, Identity: "op"},
		Resource: Resource{Kind: "FleetZone", Namespace: ns, Name: "z"},
		Outcome:  OutcomeAllowed,
	}
}

// writeSession opens the log at path, records n entries for ns, and closes — one
// "process lifetime". resume selects whether that session continues the existing chain.
func writeSession(t *testing.T, path, ns string, n int, resume bool) {
	t.Helper()
	s, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	l := New(s, "v0.3.0")
	if resume {
		if err := l.Resume(); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := l.Record(resumeEntry(ns, EventEstopTriggered)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// THE HEADLINE. Three entries, restart, two more — one continuous, verifiable chain.
func TestResume_ChainSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	writeSession(t, path, "warehouse-a", 3, false) // first boot: nothing to resume
	writeSession(t, path, "warehouse-a", 2, true)  // restart: continue the chain

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("read %d entries, want 5", len(got))
	}
	for i, e := range got {
		if e.SequenceNumber != uint64(i+1) {
			t.Errorf("entry %d has sequence %d, want %d — the restart opened a second chain",
				i, e.SequenceNumber, i+1)
		}
	}
	if res := Verify(nsEntries(got, "warehouse-a")); !res.OK {
		t.Fatalf("chain failed Verify across a restart: %+v — an untouched log reporting "+
			"TAMPERED destroys the evidentiary value of the whole audit trail", res)
	}
}

// The control. Without Resume the same flow produces the defect, so the test above is
// asserting something Resume actually causes rather than something that was always true.
func TestResume_WithoutResumeTheChainBreaksAtTheSeam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	writeSession(t, path, "warehouse-a", 3, false)
	writeSession(t, path, "warehouse-a", 2, false) // restart WITHOUT resuming

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if res := Verify(nsEntries(got, "warehouse-a")); res.OK {
		t.Fatal("a chain restarted at genesis mid-file verified as intact; " +
			"TestResume_ChainSurvivesARestart is not proving anything")
	}
}

// Per-namespace: each chain resumes from its own tail, not from a global maximum.
func TestResume_EachNamespaceResumesItsOwnChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	writeSession(t, path, "warehouse-a", 3, false)
	writeSession(t, path, "warehouse-b", 1, true)
	writeSession(t, path, "warehouse-a", 1, true)
	writeSession(t, path, "warehouse-b", 2, true)

	got, err := ReadEntries(path)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	for _, ns := range []string{"warehouse-a", "warehouse-b"} {
		chain := nsEntries(got, ns)
		if res := Verify(chain); !res.OK {
			t.Errorf("namespace %q chain failed Verify: %+v", ns, res)
		}
	}
	if n := len(nsEntries(got, "warehouse-a")); n != 4 {
		t.Errorf("warehouse-a has %d entries, want 4", n)
	}
	if n := len(nsEntries(got, "warehouse-b")); n != 3 {
		t.Errorf("warehouse-b has %d entries, want 3", n)
	}
}

// A torn final line is the expected shape of a crash DURING append: the fsync landed
// partially, or not at all. Tail must skip it rather than fail, because refusing to start
// over an unparseable byte range turns a recoverable crash into an outage — and the
// control plane that cannot start is the one that cannot record safety events either.
func TestTail_TornFinalLineIsSkippedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	writeSession(t, path, "warehouse-a", 3, false)

	// Simulate the crash: a half-written record with no closing brace and no newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for tearing: %v", err)
	}
	if _, err := f.WriteString(`{"sequence_number":4,"event_type":"ESTOP_TRIG`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	_ = f.Close()

	s, err := NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	defer func() { _ = s.Close() }()

	tail, err := s.Tail()
	if err != nil {
		t.Fatalf("Tail must not fail on a torn final line, got: %v", err)
	}
	last, ok := tail["warehouse-a"]
	if !ok {
		t.Fatal("Tail lost the namespace entirely because of one torn line")
	}
	if last.SequenceNumber != 3 {
		t.Errorf("tail sequence = %d, want 3 (the last INTACT entry)", last.SequenceNumber)
	}
}

// After that crash the control plane restarts and keeps going. The intact entries must
// form one continuous chain across the torn seam — the torn bytes are not a chain link,
// so sequence 4 chains onto sequence 3.
func TestResume_ContinuesFromTheLastIntactEntryAfterATear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	writeSession(t, path, "warehouse-a", 3, false)

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString("{\"sequence_number\":4,\"event_ty\n") // torn, but newline-terminated
	_ = f.Close()

	writeSession(t, path, "warehouse-a", 2, true)

	// ReadEntries is strict (it is an integrity reader), so read line-wise and keep the
	// parseable entries — the same set an operator gets after dropping the torn line.
	intact := parseableEntries(t, path)
	if len(intact) != 5 {
		t.Fatalf("recovered %d intact entries, want 5", len(intact))
	}
	for i, e := range intact {
		if e.SequenceNumber != uint64(i+1) {
			t.Errorf("entry %d sequence = %d, want %d — the tear opened a second chain",
				i, e.SequenceNumber, i+1)
		}
	}
	if res := Verify(intact); !res.OK {
		t.Fatalf("intact entries do not chain across the torn seam: %+v", res)
	}
}

// A sink that cannot be read back reports it rather than silently restarting at genesis.
func TestResume_NonResumableSinkIsReported(t *testing.T) {
	if err := New(&plainSink{}, "v").Resume(); !errors.Is(err, ErrSinkNotResumable) {
		t.Fatalf("Resume on a non-resumable sink = %v, want ErrSinkNotResumable", err)
	}
	// MemorySink IS resumable (it grew Tail in the same change), so dev/test deployments
	// do not silently take the genesis-restart behaviour either.
	m := &MemorySink{}
	l := New(m, "v")
	if _, err := l.Record(resumeEntry("n", EventEstopTriggered)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := New(m, "v").Resume(); err != nil {
		t.Fatalf("MemorySink must be resumable, got: %v", err)
	}
}

// plainSink implements Sink but NOT ResumableSink.
type plainSink struct{}

func (*plainSink) Append(Entry) error { return nil }

// parseableEntries reads the log line-wise and keeps the entries that parse, which is
// what an operator has after a crash left a torn line: the intact records are all still
// there and still chained. ReadEntries and the CLI's reader are deliberately STRICTER —
// they fail on an unparseable line rather than skipping it, because an integrity tool
// that silently drops bytes is not one. Recovery and verification want different answers
// here, so the difference is exercised rather than papered over.
func parseableEntries(t *testing.T, path string) []Entry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var out []Entry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
