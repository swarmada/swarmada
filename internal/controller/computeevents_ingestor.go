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
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"google.golang.org/protobuf/types/known/structpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/computeclient"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// This file is the control plane's consumer of
// compute.v1.ComputeRuntimeService.StreamStatus: it turns the semantic events a
// Service-contract executor raises into FleetAction.status.events[], which is
// what the flow evaluator's event arms and a fromMember event binding read.
//
// RA-1 IS THE ENTIRE RISK HERE, AND THIS IS ITS WORST CASE. A recognition
// service emits continuously; an append per received frame would be a status
// write per telemetry tick, at the highest rate anything in this system
// produces. The containment is edge-triggering on seq — the wire's own
// documented idempotency key — applied in applyTaskEvents below and proved in
// computeevents_ingestor_test.go: N identical frames move no resourceVersion
// and emit no Event.

const (
	// eventRetentionMax bounds status.events[]. It is the API's bound, not a
	// tunable: the CRD's MaxItems on FleetActionStatus.Events is the same
	// number, and a writer that exceeded it would have its status patch
	// rejected outright rather than trimmed. See that field's godoc for what a
	// consumer is and is not guaranteed by it.
	eventRetentionMax = 32

	// eventPayloadMaxEntries / eventPayloadMaxValueBytes mirror the CRD's
	// MaxProperties and per-value CEL bound on ActionEvent.Payload. They are
	// enforced HERE as well, at ingest, because an executor that raises one fat
	// payload would otherwise have its event rejected by the API server as part
	// of a whole status patch — wedging every LATER event behind one malformed
	// one. Trimming a payload loses a field; refusing the patch loses the run.
	eventPayloadMaxEntries    = 16
	eventPayloadMaxValueBytes = 256
)

// EventActionEventTerminal is emitted once, when a Service-contract run reports
// the event that ends it. It is the ONLY Kubernetes Event this ingestor emits:
// one per observation would be the same per-tick write RA-1 prohibits on
// status, moved to a different API.
const EventActionEventTerminal = "ActionEventTerminal"

// ComputeEventIngestor applies TaskEvents received over StreamStatus to a
// FleetAction's status.events[].
type ComputeEventIngestor struct {
	client.Client
	// Recorder emits the single terminal Event. Nil-safe.
	Recorder record.EventRecorder
}

// IngestTaskEvents applies a batch of TaskEvents to the named action and
// returns whether anything was written.
//
// It is a no-op — no status patch, no Event, no error — when every event in the
// batch is a repeat or is out of order. That is the common case for a resumed
// stream and for a chatty producer, and it is what keeps this off the per-tick
// write path.
func (i *ComputeEventIngestor) IngestTaskEvents(ctx context.Context, namespace, actionName string, evs []*fav1.TaskEvent) (bool, error) {
	if actionName == "" || len(evs) == 0 {
		return false, nil
	}

	action := &fleetv1.FleetAction{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: namespace, Name: actionName}, action); err != nil {
		// An event for an unknown action is stale, not an error — the same
		// disposition ActionStatusIngestor applies to a status update for one.
		return false, client.IgnoreNotFound(err)
	}

	base := action.DeepCopy()
	if !applyTaskEvents(&action.Status, evs) {
		return false, nil
	}
	if err := i.Status().Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return false, err
	}

	if i.Recorder != nil {
		if n := len(action.Status.Events); n > 0 && action.Status.Events[n-1].Terminal {
			// Normal, not Warning: a Continuous run reporting its own end is
			// the design working. Terminal-only, so the Event rate is O(1) per
			// run rather than the producer's frame rate.
			i.Recorder.Event(action, corev1.EventTypeNormal, EventActionEventTerminal,
				fmt.Sprintf("executor reported terminal event %q (seq %d)",
					action.Status.Events[n-1].Type, action.Status.Events[n-1].Seq))
		}
	}
	return true, nil
}

// applyTaskEvents is the edge-trigger, as a pure function of status and the
// received batch. It reports whether it changed anything; a false return means
// the caller must not write.
//
// THE RULE IS STRICTLY-GREATER-THAN, AND NOTHING ELSE. An event is accepted iff
// its seq is strictly greater than status.eventSeqHigh, which:
//
//   - discards an exact repeat (the same frame delivered twice by a retrying
//     transport, or replayed by a resume that asked from too low a point);
//   - discards an out-of-order or backwards seq, rather than inserting it —
//     seq is documented as monotonic per subject, so a lower one is a
//     duplicate or a bug, and in neither case is it news;
//   - never REWRITES an accepted event. An executor that re-raises seq 7 with
//     a different payload does not get to change what the control plane already
//     recorded and a consumer may already have branched on.
//
// The high-water mark advances even when retention immediately evicts the
// event, which is what keeps since_seq monotonic across a trim.
func applyTaskEvents(status *fleetv1.FleetActionStatus, evs []*fav1.TaskEvent) bool {
	changed := false
	for _, e := range evs {
		if e == nil || e.GetType() == "" {
			continue
		}
		// A seq that does not fit an int64 cannot be compared against the
		// stored high-water mark without wrapping into a value that might
		// compare LOWER than it — treated as malformed, the same discipline
		// GenerationFromToken applies to an unrepresentable fencing token.
		if e.GetSeq() > math.MaxInt64 {
			continue
		}
		//nolint:gosec // guarded immediately above: seq <= math.MaxInt64, so the conversion cannot wrap.
		seq := int64(e.GetSeq())
		if seq <= status.EventSeqHigh {
			continue // repeat, replay or out-of-order: not news, so not a write
		}
		status.EventSeqHigh = seq
		status.Events = append(status.Events, fleetv1.ActionEvent{
			Type:     e.GetType(),
			Seq:      seq,
			TimeMs:   e.GetTimeMs(),
			Payload:  flattenEventPayload(e.GetPayload()),
			Terminal: e.GetTerminal(),
		})
		changed = true
	}
	if !changed {
		return false
	}
	if over := len(status.Events) - eventRetentionMax; over > 0 {
		// Oldest-first eviction. Events are appended in ascending seq (the
		// accept rule guarantees it), so the slice is already ordered and the
		// head is the oldest. The copy is deliberate: reslicing in place would
		// keep the evicted entries alive behind the backing array for as long
		// as the status object lives.
		status.Events = append([]fleetv1.ActionEvent{}, status.Events[over:]...)
		status.EventsDropped += int64(over)
	}
	return true
}

// flattenEventPayload turns a TaskEvent's google.protobuf.Struct payload into
// the scalar map ActionEvent.Payload is, applying the CRD's own bounds at
// ingest.
//
// A NON-SCALAR ENTRY IS DROPPED, NOT ENCODED. JSON-encoding a nested object into
// a string would give a select's `equals` something it can compare and a
// fromMember binding something it can pass on, both of which would be a value
// the author never wrote and cannot predict the spelling of. Dropping it means
// `present: false` — which is true, and is the one answer a consumer can act on.
//
// A null is dropped for the reason the proto drops a defaulted safety scalar: a
// null is "no reading", not a reading of nothing.
func flattenEventPayload(s *structpb.Struct) map[string]string {
	if s == nil || len(s.GetFields()) == 0 {
		return nil
	}
	// Sorted, so which entries survive the 16-entry bound is deterministic: a
	// map's iteration order is random, and a payload that kept a different
	// subset on each delivery would make the same event mean different things.
	keys := make([]string, 0, len(s.GetFields()))
	for k := range s.GetFields() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if len(out) >= eventPayloadMaxEntries {
			break
		}
		v, ok := scalarString(s.GetFields()[k])
		if !ok || len(v) > eventPayloadMaxValueBytes {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scalarString renders one Struct value as the string encoding the API stores,
// or reports that it is not a scalar.
func scalarString(v *structpb.Value) (string, bool) {
	switch k := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return k.StringValue, true
	case *structpb.Value_NumberValue:
		// -1 precision: the shortest representation that round-trips, so an
		// integral confidence of 1 reads as "1" and not "1.000000" — the value
		// an author writes in `equals:` is the value they see in kubectl.
		return strconv.FormatFloat(k.NumberValue, 'f', -1, 64), true
	case *structpb.Value_BoolValue:
		return strconv.FormatBool(k.BoolValue), true
	default:
		return "", false // null, struct, list
	}
}

// PumpStatusStream opens the run's StreamStatus and ingests events until the
// stream ends, the context is cancelled, or an error that is not io.EOF occurs.
// It returns nil on a clean end of stream (the executor closed it) and on
// context cancellation.
//
// RESUMPTION. The since_seq handed to the executor is read from the action's
// PERSISTED status.eventSeqHigh, not from an in-memory cursor, so a control-plane
// restart resumes where the last durable write left off rather than re-ingesting
// from the start. Re-ingesting would be harmless (the accept rule discards every
// repeat) but would not be free: it is a full stream's worth of frames the
// executor need never send.
func (i *ComputeEventIngestor) PumpStatusStream(
	ctx context.Context, c *computeclient.Client, namespace, actionName, invocationID string,
) error {
	action := &fleetv1.FleetAction{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: namespace, Name: actionName}, action); err != nil {
		return client.IgnoreNotFound(err)
	}
	since := action.Status.EventSeqHigh
	if since < 0 {
		since = 0
	}

	stream, err := c.StreamStatus(ctx, invocationID, uint64(since))
	if err != nil {
		return err
	}
	for {
		e, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			if ctx.Err() != nil {
				return nil // our own cancellation, not the executor's failure
			}
			return fmt.Errorf("receiving task event for %q: %w", actionName, err)
		}
		// One event per patch attempt, and most attempts write nothing: the
		// accept rule inside IngestTaskEvents is what makes a per-frame CALL
		// safe, not a per-frame WRITE. Batching here would only delay the
		// first genuinely new event, which is the one a select is waiting for.
		if _, err := i.IngestTaskEvents(ctx, namespace, actionName, []*fav1.TaskEvent{e}); err != nil {
			log.FromContext(ctx).Error(err, "ingesting task event", "action", actionName, "seq", e.GetSeq())
		}
	}
}
