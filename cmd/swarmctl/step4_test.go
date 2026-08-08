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
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/cli"
)

func clientKey(ns, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: ns, Name: name}
}

// exportedChain builds a real, sealed two-entry chain via internal/audit and
// returns it as newline-delimited JSON, the format `swarmctl audit` consumes.
func exportedChain(t *testing.T) []audit.Entry {
	t.Helper()
	log := audit.New(&audit.MemorySink{}, "v0.1.0")
	e1, err := log.Record(audit.Entry{
		EventType: audit.EventRobotAdmitted, Namespace: "warehouse-a",
		Actor:    audit.Actor{Type: audit.ActorUser, Identity: "alice"},
		Resource: audit.Resource{Kind: "DiscoveredRobot", Namespace: "warehouse-a", Name: "dr-1"},
		Action:   "admit", Outcome: audit.OutcomeAllowed,
	})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := log.Record(audit.Entry{
		EventType: audit.EventEstopTriggered, Namespace: "warehouse-a",
		Actor:    audit.Actor{Type: audit.ActorUser, Identity: "bob"},
		Resource: audit.Resource{Kind: "FleetZone", Namespace: "warehouse-a", Name: "zone-b3"},
		Action:   "estop-trigger", Outcome: audit.OutcomeAllowed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return []audit.Entry{e1, e2}
}

func toJSONL(t *testing.T, entries []audit.Entry) string {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func auditOptions(in string, out *bytes.Buffer, format cli.OutputFormat) *options {
	return &options{
		streams:   cli.IOStreams{In: strings.NewReader(in), Out: out, Err: out},
		outputFmt: format,
	}
}

func TestAuditLogRendersChain(t *testing.T) {
	jsonl := toJSONL(t, exportedChain(t))
	var out bytes.Buffer
	o := auditOptions(jsonl, &out, cli.OutputTable)
	entries, err := o.readAuditChain("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if err := o.printAuditLog(entries); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"SEQ", "EVENT", "OUTCOME", "ROBOT_ADMITTED", "ESTOP_TRIGGERED", "alice", "FleetZone/zone-b3", "Allowed"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit log missing %q:\n%s", want, got)
		}
	}
}

func TestAuditVerifyGoodChainPasses(t *testing.T) {
	jsonl := toJSONL(t, exportedChain(t))
	var out bytes.Buffer
	o := auditOptions(jsonl, &out, cli.OutputTable)
	entries, _ := o.readAuditChain("")
	if err := o.verifyAuditChain(entries); err != nil {
		t.Fatalf("an intact chain must verify, got: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "OK") {
		t.Errorf("expected OK verdict:\n%s", out.String())
	}
}

func TestAuditVerifyDetectsTamper(t *testing.T) {
	entries := exportedChain(t)
	// Tamper with a sealed entry's action WITHOUT recomputing its chain hash —
	// exactly what an attacker editing the exported log would leave behind.
	entries[1].Action = "estop-clear"
	jsonl := toJSONL(t, entries)

	var out bytes.Buffer
	o := auditOptions(jsonl, &out, cli.OutputTable)
	parsed, _ := o.readAuditChain("")
	err := o.verifyAuditChain(parsed)
	if err == nil {
		t.Fatalf("verify must fail on a tampered chain\n%s", out.String())
	}
	if !strings.Contains(out.String(), "TAMPERED") {
		t.Errorf("expected TAMPERED verdict:\n%s", out.String())
	}
}

func TestReAdmitAppliesClassTemplate(t *testing.T) {
	ns := "warehouse-a"
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-007", Namespace: ns},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "Origin", Zone: "zone-b3", RobotClass: "old-class",
			Adapter:  fleetv1.AdapterRef{Name: "old-adapter", Version: "1.0"},
			Charging: &fleetv1.RobotChargingConfig{DockName: "dock-42"},
		},
	}
	class := robotClass("new-class", ns)
	class.Spec.BaseAdapter = fleetv1.BaseAdapterRef{Name: "acme-adapter", Version: "2.0"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(robot, class).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.reAdmit(context.Background(), c, ns, "amr-007", "new-class", "", true); err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	got := &fleetv1.Robot{}
	_ = c.Get(context.Background(), clientKey(ns, "amr-007"), got)
	if got.Spec.RobotClass != "new-class" || got.Spec.Adapter.Name != "acme-adapter" || got.Spec.Adapter.Version != "2.0" {
		t.Errorf("class not applied: %+v", got.Spec)
	}
	// Identity, zone, and the charging dock are preserved.
	if got.Spec.Manufacturer != "Acme" || got.Spec.Zone != "zone-b3" {
		t.Errorf("identity/zone should be preserved: %+v", got.Spec)
	}
	if got.Spec.Charging == nil || got.Spec.Charging.DockName != "dock-42" {
		t.Errorf("charging dock should be preserved: %+v", got.Spec.Charging)
	}
}

func TestRobotClassRolloutReAdmitsMatching(t *testing.T) {
	ns := "warehouse-a"
	class := robotClass("fleet-class", ns)
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns}, Spec: fleetv1.RobotSpec{RobotClass: "fleet-class", Zone: "z1", Adapter: fleetv1.AdapterRef{Name: "a"}}}
	r2 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: ns}, Spec: fleetv1.RobotSpec{RobotClass: "fleet-class", Zone: "z2", Adapter: fleetv1.AdapterRef{Name: "a"}}}
	other := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r3", Namespace: ns}, Spec: fleetv1.RobotSpec{RobotClass: "different", Zone: "z1", Adapter: fleetv1.AdapterRef{Name: "a"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(class, r1, r2, other).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.robotClassRollout(context.Background(), c, ns, "fleet-class", "", true); err != nil {
		t.Fatalf("rollout: %v", err)
	}
	// r1 and r2 picked up the class adapter; r3 (other class) is untouched.
	for _, name := range []string{"r1", "r2"} {
		got := &fleetv1.Robot{}
		_ = c.Get(context.Background(), clientKey(ns, name), got)
		if got.Spec.Adapter.Name != "acme-adapter" {
			t.Errorf("%s not rolled out: adapter=%s", name, got.Spec.Adapter.Name)
		}
	}
	got3 := &fleetv1.Robot{}
	_ = c.Get(context.Background(), clientKey(ns, "r3"), got3)
	if got3.Spec.Adapter.Name != "a" {
		t.Errorf("r3 (different class) should be untouched, adapter=%s", got3.Spec.Adapter.Name)
	}
	if !strings.Contains(out.String(), "2 re-admitted, 0 failed") {
		t.Errorf("expected rollout summary, got:\n%s", out.String())
	}
}

func TestRobotClassRolloutZoneFilter(t *testing.T) {
	ns := "warehouse-a"
	class := robotClass("fleet-class", ns)
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns}, Spec: fleetv1.RobotSpec{RobotClass: "fleet-class", Zone: "z1", Adapter: fleetv1.AdapterRef{Name: "a"}}}
	r2 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: ns}, Spec: fleetv1.RobotSpec{RobotClass: "fleet-class", Zone: "z2", Adapter: fleetv1.AdapterRef{Name: "a"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(class, r1, r2).Build()
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)

	if err := o.robotClassRollout(context.Background(), c, ns, "fleet-class", "z1", true); err != nil {
		t.Fatalf("rollout: %v", err)
	}
	if !strings.Contains(out.String(), "1 re-admitted") {
		t.Errorf("zone filter should limit to r1 only:\n%s", out.String())
	}
	got2 := &fleetv1.Robot{}
	_ = c.Get(context.Background(), clientKey(ns, "r2"), got2)
	if got2.Spec.Adapter.Name != "a" {
		t.Errorf("r2 (zone z2) should be excluded by --zone z1")
	}
}
