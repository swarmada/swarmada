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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/protobuf/types/known/structpb"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// RA-1 IS THE WHOLE POINT OF THIS FILE. A recognition service emits
// continuously, so an append per received frame would be a status write per
// telemetry tick — the highest-rate defect this invariant exists to prevent. The
// containment is edge-triggering on seq, and the proof is the one L8 used for
// containment: N identical frames move no resourceVersion and emit no Event.
//
// A test that only shows the happy-path append does not demonstrate this, so the
// happy path is here as the CONTROL — it is what makes the unmoved
// resourceVersion mean "nothing was written" rather than "nothing works".

const evNS = "warehouse-a"

func newComputeEventIngestor(t *testing.T, objs ...client.Object) (*ComputeEventIngestor, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAction{}).
		Build()
	rec := record.NewFakeRecorder(64)
	return &ComputeEventIngestor{Client: c, Recorder: rec}, c, rec
}

func serviceAction(name string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: evNS},
		Spec: fleetv1.FleetActionSpec{
			Executor: &fleetv1.ActionExecutor{EndpointRef: "intruder-detector"},
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress},
	}
}

func taskEvent(seq uint64, evType string, payload map[string]any, terminal bool) *fav1.TaskEvent {
	e := &fav1.TaskEvent{Type: evType, Seq: seq, TimeMs: 1700000000000, Terminal: terminal}
	if payload != nil {
		s, err := structpb.NewStruct(payload)
		if err != nil {
			panic(err)
		}
		e.Payload = s
	}
	return e
}

func getEvAction(t *testing.T, c client.Client, name string) *fleetv1.FleetAction {
	t.Helper()
	var a fleetv1.FleetAction
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: evNS, Name: name}, &a); err != nil {
		t.Fatalf("get %q: %v", name, err)
	}
	return &a
}

func drainRecorder(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// ── THE RA-1 EVIDENCE ────────────────────────────────────────────────────────

// N identical frames after the first move no resourceVersion and emit no Event.
//
// N = 64: twice the retention bound, so that if the append were NOT
// edge-triggered the repeats would not merely write 64 times, they would also
// have evicted the original event and moved eventsDropped — two independent
// ways for the assertion below to catch a regression rather than one.
func TestComputeEvents_RA1_IdenticalFramesWriteNothing(t *testing.T) {
	const n = 64
	i, c, rec := newComputeEventIngestor(t, serviceAction("watch"))
	ctx := context.Background()

	frame := taskEvent(1, "IntruderDetected", map[string]any{"zone": "dock"}, false)

	// The CONTROL: the first delivery is genuinely new, so it writes.
	wrote, err := i.IngestTaskEvents(ctx, evNS, "watch", []*fav1.TaskEvent{frame})
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !wrote {
		t.Fatal("the first delivery of a new seq must write — otherwise the no-op assertions below prove nothing")
	}
	after := getEvAction(t, c, "watch")
	rvAfterFirst := after.ResourceVersion
	if len(after.Status.Events) != 1 || after.Status.EventSeqHigh != 1 {
		t.Fatalf("after the first frame: events=%v eventSeqHigh=%d", after.Status.Events, after.Status.EventSeqHigh)
	}
	if evs := drainRecorder(rec); len(evs) != 0 {
		t.Fatalf("a non-terminal observation must emit no Kubernetes Event; got %v", evs)
	}

	// THE PROOF: N more deliveries of the same frame.
	for k := 0; k < n; k++ {
		wrote, err := i.IngestTaskEvents(ctx, evNS, "watch", []*fav1.TaskEvent{frame})
		if err != nil {
			t.Fatalf("repeat %d: %v", k, err)
		}
		if wrote {
			t.Fatalf("repeat %d reported a write; an already-accepted seq must write nothing", k)
		}
	}

	final := getEvAction(t, c, "watch")
	if final.ResourceVersion != rvAfterFirst {
		t.Fatalf("resourceVersion moved from %s to %s across %d identical frames — that is a status write per telemetry tick, which RA-1 prohibits",
			rvAfterFirst, final.ResourceVersion, n)
	}
	if evs := drainRecorder(rec); len(evs) != 0 {
		t.Fatalf("%d identical frames emitted %d Kubernetes Events: %v", n, len(evs), evs)
	}
	if len(final.Status.Events) != 1 || final.Status.EventSeqHigh != 1 || final.Status.EventsDropped != 0 {
		t.Fatalf("state changed under repeats: events=%d seqHigh=%d dropped=%d",
			len(final.Status.Events), final.Status.EventSeqHigh, final.Status.EventsDropped)
	}

	// And the control again, at the end: a genuinely NEW seq still writes, so
	// the unmoved resourceVersion above was containment and not a dead ingestor.
	wrote, err = i.IngestTaskEvents(ctx, evNS, "watch", []*fav1.TaskEvent{taskEvent(2, "IntruderDetected", nil, false)})
	if err != nil {
		t.Fatalf("new seq after repeats: %v", err)
	}
	if !wrote {
		t.Fatal("a new seq after the repeats must still write")
	}
	if got := getEvAction(t, c, "watch"); got.ResourceVersion == rvAfterFirst {
		t.Fatal("resourceVersion did not move for a genuinely new event — the ingestor is inert, not contained")
	}
}

// A lower or equal seq is discarded, and specifically does NOT rewrite the
// event already recorded. A consumer may already have branched on it.
func TestComputeEvents_OutOfOrderSeqNeitherAppendsNorRewrites(t *testing.T) {
	i, c, _ := newComputeEventIngestor(t, serviceAction("watch"))
	ctx := context.Background()

	mustIngest(t, i, "watch", taskEvent(5, "IntruderDetected", map[string]any{"zone": "dock"}, false))
	rv := getEvAction(t, c, "watch").ResourceVersion

	for _, e := range []*fav1.TaskEvent{
		taskEvent(4, "IntruderDetected", map[string]any{"zone": "aisle"}, false), // older
		taskEvent(5, "IntruderDetected", map[string]any{"zone": "aisle"}, false), // same seq, different payload
		taskEvent(0, "IntruderDetected", map[string]any{"zone": "aisle"}, false), // from-the-start replay
	} {
		wrote, err := i.IngestTaskEvents(ctx, evNS, "watch", []*fav1.TaskEvent{e})
		if err != nil {
			t.Fatalf("ingest seq %d: %v", e.GetSeq(), err)
		}
		if wrote {
			t.Fatalf("seq %d wrote; only a strictly greater seq is news", e.GetSeq())
		}
	}

	got := getEvAction(t, c, "watch")
	if got.ResourceVersion != rv {
		t.Fatalf("resourceVersion moved on discarded events: %s -> %s", rv, got.ResourceVersion)
	}
	if len(got.Status.Events) != 1 || got.Status.Events[0].Payload["zone"] != "dock" {
		t.Fatalf("an accepted event was rewritten by a repeat of its seq: %+v", got.Status.Events)
	}
}

// ── retention ────────────────────────────────────────────────────────────────

// The window is bounded at 32; the high-water mark is not; and eventsDropped
// makes the loss VISIBLE rather than silent. Without the counter, "never raised"
// and "raised and evicted" are the same observation to a consumer.
func TestComputeEvents_RetentionBoundsTheWindowButNotTheHighWaterMark(t *testing.T) {
	i, c, _ := newComputeEventIngestor(t, serviceAction("watch"))

	const total = 50
	for seq := 1; seq <= total; seq++ {
		mustIngest(t, i, "watch", taskEvent(uint64(seq), "IntruderDetected", nil, false))
	}

	got := getEvAction(t, c, "watch")
	if len(got.Status.Events) != eventRetentionMax {
		t.Fatalf("retained %d events, want the bound %d", len(got.Status.Events), eventRetentionMax)
	}
	if got.Status.EventSeqHigh != total {
		t.Fatalf("eventSeqHigh = %d, want %d — the high-water mark must survive a trim, or since_seq walks backwards on reconnect",
			got.Status.EventSeqHigh, total)
	}
	if want := int64(total - eventRetentionMax); got.Status.EventsDropped != want {
		t.Fatalf("eventsDropped = %d, want %d", got.Status.EventsDropped, want)
	}
	// Oldest-first: the survivors are the most recent, in ascending seq.
	if got.Status.Events[0].Seq != int64(total-eventRetentionMax+1) {
		t.Fatalf("oldest retained seq = %d, want %d", got.Status.Events[0].Seq, total-eventRetentionMax+1)
	}
	for k := 1; k < len(got.Status.Events); k++ {
		if got.Status.Events[k].Seq <= got.Status.Events[k-1].Seq {
			t.Fatalf("retained events are not ascending by seq: %+v", got.Status.Events)
		}
	}
}

// ── payload flattening ───────────────────────────────────────────────────────

// A non-scalar payload entry is DROPPED, not JSON-encoded. Encoding it would
// give a select's `equals` something to compare and a parameter binding
// something to pass on — both a value the author never wrote and cannot predict
// the spelling of. Dropping it means `present: false`, which is true.
func TestComputeEvents_PayloadKeepsScalarsAndDropsTheRest(t *testing.T) {
	i, c, _ := newComputeEventIngestor(t, serviceAction("watch"))

	mustIngest(t, i, "watch", taskEvent(1, "IntruderDetected", map[string]any{
		"zone":       "dock",
		"confidence": 0.91,
		"integral":   1.0,
		"armed":      true,
		"bbox":       []any{1.0, 2.0, 3.0, 4.0},
		"nested":     map[string]any{"a": "b"},
		"unknown":    nil,
	}, false))

	p := getEvAction(t, c, "watch").Status.Events[0].Payload
	want := map[string]string{"zone": "dock", "confidence": "0.91", "integral": "1", "armed": "true"}
	for k, v := range want {
		if p[k] != v {
			t.Fatalf("payload[%q] = %q, want %q", k, p[k], v)
		}
	}
	for _, k := range []string{"bbox", "nested", "unknown"} {
		if _, present := p[k]; present {
			t.Fatalf("non-scalar payload entry %q survived as %q; it must be dropped, not encoded", k, p[k])
		}
	}
}

// An over-long value is dropped rather than truncated, and the event is still
// accepted. The CRD's per-value CEL bound would otherwise reject the WHOLE
// status patch, wedging every later event behind one fat payload.
func TestComputeEvents_OverLongPayloadValueDoesNotWedgeTheRun(t *testing.T) {
	i, c, _ := newComputeEventIngestor(t, serviceAction("watch"))

	long := make([]byte, eventPayloadMaxValueBytes+1)
	for k := range long {
		long[k] = 'x'
	}
	mustIngest(t, i, "watch", taskEvent(1, "IntruderDetected", map[string]any{
		"zone": "dock", "blob": string(long),
	}, false))
	mustIngest(t, i, "watch", taskEvent(2, "IntruderDetected", map[string]any{"zone": "aisle"}, false))

	got := getEvAction(t, c, "watch")
	if len(got.Status.Events) != 2 {
		t.Fatalf("a fat payload blocked a later event: %+v", got.Status.Events)
	}
	if _, present := got.Status.Events[0].Payload["blob"]; present {
		t.Fatal("an over-long value was kept; the CRD's CEL bound would reject the status patch")
	}
	if got.Status.Events[0].Payload["zone"] != "dock" {
		t.Fatal("the rest of the payload was lost with the over-long entry")
	}
}

// ── the one Event ────────────────────────────────────────────────────────────

// The terminal event emits exactly one Kubernetes Event — O(1) per run, not one
// per observation, which would be the same per-tick write moved to another API.
func TestComputeEvents_OnlyTheTerminalEventIsRecorded(t *testing.T) {
	i, _, rec := newComputeEventIngestor(t, serviceAction("watch"))

	for seq := 1; seq <= 5; seq++ {
		mustIngest(t, i, "watch", taskEvent(uint64(seq), "IntruderDetected", nil, false))
	}
	if evs := drainRecorder(rec); len(evs) != 0 {
		t.Fatalf("5 ordinary observations emitted %d Events: %v", len(evs), evs)
	}

	mustIngest(t, i, "watch", taskEvent(6, "WatchEnded", nil, true))
	evs := drainRecorder(rec)
	if len(evs) != 1 {
		t.Fatalf("the terminal event must emit exactly one Event; got %d: %v", len(evs), evs)
	}
	if !strings.Contains(evs[0], EventActionEventTerminal) || !strings.Contains(evs[0], "WatchEnded") {
		t.Fatalf("the terminal Event must name itself and the event type; got %q", evs[0])
	}
}

// An event for an action that does not exist is stale, not an error.
func TestComputeEvents_UnknownActionIsNotAnError(t *testing.T) {
	i, _, _ := newComputeEventIngestor(t)
	wrote, err := i.IngestTaskEvents(context.Background(), evNS, "gone",
		[]*fav1.TaskEvent{taskEvent(1, "IntruderDetected", nil, false)})
	if err != nil || wrote {
		t.Fatalf("wrote=%v err=%v, want no write and no error", wrote, err)
	}
}

func mustIngest(t *testing.T, i *ComputeEventIngestor, action string, e *fav1.TaskEvent) {
	t.Helper()
	wrote, err := i.IngestTaskEvents(context.Background(), evNS, action, []*fav1.TaskEvent{e})
	if err != nil {
		t.Fatalf("ingest seq %d: %v", e.GetSeq(), err)
	}
	if !wrote {
		t.Fatalf("ingest seq %d wrote nothing; it was expected to be new", e.GetSeq())
	}
}
