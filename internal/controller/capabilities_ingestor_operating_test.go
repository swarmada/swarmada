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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FA-R1 — declare. The adapter reports the operating subjects it can honour for a robot on
// CapabilitiesSnapshot.honoured_operating_constraints, and the control plane projects it
// read-only to Robot.status.honouredOperatingConstraints[] — "the same path sensor_streams
// already takes" (rfcs/amendments/RFC-0001-zone-restrictions.md, Part D).
//
// Structurally identical to TestCapabilitiesIngestor_ProjectsSensorStreams beside it, and
// deliberately so: a second, subtly different projection path for the same kind of data is how
// the next reader learns the wrong lesson.

func operatingRobot(ns, name string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: name},
		},
		Spec: fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
	}
}

func TestCapabilitiesIngestor_ProjectsHonouredOperatingConstraints(t *testing.T) {
	ns := "ward-3"
	i, c := newCapIngestor(t, operatingRobot(ns, "ward-robot-1"))

	snap := &fav1.CapabilitiesSnapshot{
		RobotId:                      "ward-robot-1",
		HonouredOperatingConstraints: []string{"camera", "microphone", "example.com/leaf-blower"},
	}
	if err := i.IngestCapabilities(context.Background(), ns, snap); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "ward-robot-1"}, got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	want := []string{"camera", "microphone", "example.com/leaf-blower"}
	if len(got.Status.HonouredOperatingConstraints) != len(want) {
		t.Fatalf("want %d honoured subjects, got %d: %+v",
			len(want), len(got.Status.HonouredOperatingConstraints), got.Status.HonouredOperatingConstraints)
	}
	for n, w := range want {
		if got.Status.HonouredOperatingConstraints[n] != w {
			t.Fatalf("subject %d: want %q, got %q — the adapter's ORDER is preserved so a declaration "+
				"round-trips verbatim: %+v", n, w, got.Status.HonouredOperatingConstraints[n],
				got.Status.HonouredOperatingConstraints)
		}
	}
}

// ADR-0058 §7: the subject vocabulary is NOT the capability vocabulary, and an adapter that
// reports something outside it has declared nothing the schema can hold. Dropping it is
// FAIL-CLOSED and is the only answer consistent with ADR-0058 Amendment 1 §A1.2 ("an empty or
// absent honouredOperatingConstraints[] satisfies no subject"): the robot is withheld for that
// subject, exactly as if the adapter had stayed silent.
//
// Dropping rather than rejecting the whole patch matters: the CRD carries the same rule as CEL,
// so an unfiltered projection would make ONE malformed subject fail the entire status write and
// cost the robot its valid declarations too.
func TestCapabilitiesIngestor_DropsSubjectsOutsideTheVocabulary(t *testing.T) {
	ns := "ward-4"
	i, c := newCapIngestor(t, operatingRobot(ns, "ward-robot-2"))

	snap := &fav1.CapabilitiesSnapshot{
		RobotId: "ward-robot-2",
		HonouredOperatingConstraints: []string{
			"camera",            // core seven — kept
			"Microphone",        // capitalised: not the vocabulary
			"sensor.microphone", // a CAPABILITY-shaped name, bare: not one of the core seven
			"example.com/lidar", // namespaced extension — kept
			"/leading",          // malformed
			"",                  // empty
			"camera",            // duplicate of a kept entry
		},
	}
	if err := i.IngestCapabilities(context.Background(), ns, snap); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "ward-robot-2"}, got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	want := []string{"camera", "example.com/lidar"}
	if len(got.Status.HonouredOperatingConstraints) != len(want) {
		t.Fatalf("want exactly %v, got %v — a bare capability-shaped name is not a subject "+
			"(ADR-0058 §7), and a listType=set may not carry a duplicate",
			want, got.Status.HonouredOperatingConstraints)
	}
	for n, w := range want {
		if got.Status.HonouredOperatingConstraints[n] != w {
			t.Fatalf("want %v, got %v", want, got.Status.HonouredOperatingConstraints)
		}
	}
}

// RA-1, and this is the exact place it gets violated: a CapabilitiesSnapshot arrives on a
// STREAM, so projecting it on every arrival would be a per-tick status write.
//
// The existing sensor-stream test asserts only that a second ingest returns no error, which a
// controller that rewrote status on every tick would also pass. This asserts the actual
// discipline: after an unchanged snapshot the object's resourceVersion has not moved, which for
// the fake client (as for the API server) is true only if no write was issued.
func TestCapabilitiesIngestor_UnchangedTickWritesNoStatus(t *testing.T) {
	ns := "ward-5"
	i, c := newCapIngestor(t, operatingRobot(ns, "ward-robot-3"))
	ctx := context.Background()
	key := client.ObjectKey{Namespace: ns, Name: "ward-robot-3"}

	snap := &fav1.CapabilitiesSnapshot{
		RobotId:                      "ward-robot-3",
		HonouredOperatingConstraints: []string{"camera", "speed"},
		SensorStreams:                []*fav1.SensorStream{{StreamId: "front-rgb", MediaType: "image/jpeg"}},
	}
	if err := i.IngestCapabilities(ctx, ns, snap); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	after := &fleetv1.Robot{}
	if err := c.Get(ctx, key, after); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	settled := after.ResourceVersion
	if len(after.Status.HonouredOperatingConstraints) != 2 {
		t.Fatalf("the first ingest did not project: %+v", after.Status.HonouredOperatingConstraints)
	}

	// Ten more identical ticks — a stream delivering the same snapshot, which is what a
	// heartbeat-driven adapter does.
	for n := 0; n < 10; n++ {
		if err := i.IngestCapabilities(ctx, ns, snap); err != nil {
			t.Fatalf("tick %d: %v", n, err)
		}
	}
	quiet := &fleetv1.Robot{}
	if err := c.Get(ctx, key, quiet); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if quiet.ResourceVersion != settled {
		t.Fatalf("RA-1 VIOLATED: resourceVersion moved %s -> %s across 10 unchanged snapshots. "+
			"A CapabilitiesSnapshot arrives on a telemetry stream; projecting it must write status "+
			"only when the declaration CHANGED (AGENTS.md, status-write discipline).",
			settled, quiet.ResourceVersion)
	}

	// And a snapshot that DOES change still lands — otherwise the test above passes trivially
	// for a projection that never writes at all.
	changed := &fav1.CapabilitiesSnapshot{
		RobotId:                      "ward-robot-3",
		HonouredOperatingConstraints: []string{"camera", "speed", "noise"},
		SensorStreams:                snap.GetSensorStreams(),
	}
	if err := i.IngestCapabilities(ctx, ns, changed); err != nil {
		t.Fatalf("changed ingest: %v", err)
	}
	moved := &fleetv1.Robot{}
	if err := c.Get(ctx, key, moved); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if moved.ResourceVersion == settled {
		t.Fatalf("a CHANGED declaration performed no write; the no-op assertion above would be vacuous")
	}
	if len(moved.Status.HonouredOperatingConstraints) != 3 {
		t.Fatalf("changed declaration not projected: %+v", moved.Status.HonouredOperatingConstraints)
	}
}

// An adapter that declares nothing projects nothing, and that is NOT a breach (FA-R1). It is
// also not a permission: ADR-0058 Amendment 1 §A1.2 rules that an empty or absent list satisfies
// no subject, so the robot is simply withheld from any zone restricting one.
func TestCapabilitiesIngestor_NoDeclarationIsNotABreach(t *testing.T) {
	ns := "ward-6"
	i, c := newCapIngestor(t, operatingRobot(ns, "ward-robot-4"))
	ctx := context.Background()

	if err := i.IngestCapabilities(ctx, ns, &fav1.CapabilitiesSnapshot{RobotId: "ward-robot-4"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ward-robot-4"}, got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if got.Status.HonouredOperatingConstraints != nil {
		t.Fatalf("an adapter that declared nothing produced %+v; absence must stay absence",
			got.Status.HonouredOperatingConstraints)
	}
}
