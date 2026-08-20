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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/cli"
)

// `swarmctl verify audit` across a control-plane restart (Round-4 D6).
//
// The unit tests in internal/audit prove Log.Resume() keeps the hash chain continuous.
// This one closes the loop on the claim that matters to an operator: the verifier they
// actually run, over a log file a restarted control plane actually produced, prints OK.
//
// It is a separate test from the audit-package ones because the CLI has its own reader
// (readAuditChain) and its own per-namespace grouping (verifyAuditChain). A chain can be
// sound and still fail here — that is precisely the seam an integration-shaped test covers.

// recordSession writes n entries for ns into the log at path through one Log lifetime,
// resuming the existing chain when asked. It models one control-plane process.
func recordSession(t *testing.T, path, ns string, n int, resume bool) {
	t.Helper()
	sink, err := audit.NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	log := audit.New(sink, "v0.3.0")
	if resume {
		if err := log.Resume(); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := log.Record(audit.Entry{
			Namespace: ns, EventType: audit.EventEstopTriggered, Action: "estop-trigger",
			Actor:    audit.Actor{Type: audit.ActorUser, Identity: "alice"},
			Resource: audit.Resource{Kind: "FleetZone", Namespace: ns, Name: "zone-b3"},
			Outcome:  audit.OutcomeAllowed,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAuditVerify_PassesAcrossAControlPlaneRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	recordSession(t, path, "warehouse-a", 3, false) // first boot
	recordSession(t, path, "warehouse-b", 2, true)  // restart, second namespace
	recordSession(t, path, "warehouse-a", 2, true)  // restart, back to the first

	var out bytes.Buffer
	o := auditOptions("", &out, cli.OutputTable)
	entries, err := o.readAuditChain(path)
	if err != nil {
		t.Fatalf("readAuditChain over a restarted log: %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("read %d entries, want 7", len(entries))
	}
	if err := o.verifyAuditChain(entries); err != nil {
		t.Fatalf("`swarmctl verify audit` FAILED on a log that was only restarted, not "+
			"tampered with: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "TAMPERED") {
		t.Errorf("verifier reported TAMPERED across a clean restart:\n%s", out.String())
	}
	for _, ns := range []string{"warehouse-a", "warehouse-b"} {
		if !strings.Contains(out.String(), "namespace="+ns) {
			t.Errorf("no verdict printed for namespace %q:\n%s", ns, out.String())
		}
	}
}

// The control: the same three sessions WITHOUT Resume produce the report an operator
// would have seen before D6 — a tamper verdict on an untouched log.
func TestAuditVerify_WithoutResumeReportsTamperedOnAnUntouchedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	recordSession(t, path, "warehouse-a", 3, false)
	recordSession(t, path, "warehouse-a", 2, false) // restart without resuming

	var out bytes.Buffer
	o := auditOptions("", &out, cli.OutputTable)
	entries, err := o.readAuditChain(path)
	if err != nil {
		t.Fatalf("readAuditChain: %v", err)
	}
	if err := o.verifyAuditChain(entries); err == nil {
		t.Fatal("a chain that restarted at genesis mid-file verified as intact; " +
			"the passing test above is not proving Resume did anything")
	}
}

// A torn final line is fatal to the VERIFIER even though Tail skips it (see
// internal/audit: TestTail_TornFinalLineIsSkippedNotFatal). Both behaviours are
// deliberate and they differ: recovery must not be blocked by unparseable bytes, and an
// integrity tool must not silently skip them. This pins the CLI half so the asymmetry is
// a documented property rather than a surprise during an incident — the error names the
// line, which is what an operator needs to excise it before re-verifying.
func TestAuditVerify_TornLineIsReportedWithItsLineNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.ndjson")
	recordSession(t, path, "warehouse-a", 3, false)
	appendRaw(t, path, "{\"sequence_number\":4,\"event_ty\n")
	recordSession(t, path, "warehouse-a", 2, true)

	var out bytes.Buffer
	o := auditOptions("", &out, cli.OutputTable)
	_, err := o.readAuditChain(path)
	if err == nil {
		t.Fatal("the verifier silently accepted a log containing an unparseable line")
	}
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("error does not name the offending line, so an operator cannot excise it: %v", err)
	}
}

// appendRaw writes bytes to the log without going through the sink, simulating the
// partial record a crash mid-append leaves behind.
func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open for tearing: %v", err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
