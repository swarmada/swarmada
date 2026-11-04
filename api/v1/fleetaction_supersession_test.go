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

package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

// RFC-0001 #fleetaction-superseded-is-not-cancelled-by-an-operator "Superseded Is Not Cancelled By An Operator": phase alone cannot
// tell a supersession from an operator cancellation, so the two causes are carried on
// their own status paths. Both must serialize under the names the chapter specifies —
// a client reads them by JSON path, not by Go field name.
func TestFleetActionStatus_CancellationCauseSerializes(t *testing.T) {
	st := FleetActionStatus{
		Phase:              ActionPhaseCancelled,
		CancellationReason: CancellationReasonSupersededByDirective,
		SupersededBy:       "restock-b",
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := back["cancellationReason"]; got != "SupersededByDirective" {
		t.Errorf("status.cancellationReason = %v, want SupersededByDirective", got)
	}
	if got := back["supersededBy"]; got != "restock-b" {
		t.Errorf("status.supersededBy = %v, want restock-b", got)
	}
}

// Both paths are omitempty: an action nobody stated a cause for carries neither key.
// RFC-0001 #fleetaction-superseded-is-not-cancelled-by-an-operator is explicit that an empty cancellationReason means "no controller stated
// one" and MUST NOT be read as evidence of either cause, so the absent case must stay
// distinguishable from a stated one.
func TestFleetActionStatus_CancellationCauseOmittedWhenUnset(t *testing.T) {
	raw, err := json.Marshal(FleetActionStatus{Phase: ActionPhaseCancelled})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"cancellationReason", "supersededBy"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("status carries %q when unset; it must be omitted", key)
		}
	}
}

// The enum is CLOSED (§9.1.4.4.2): exactly three causes, and an operator cancellation
// is a different value from a supersession — that difference is the whole point.
func TestCancellationReason_ClosedEnum(t *testing.T) {
	want := map[CancellationReason]bool{
		CancellationReasonOperator:              true,
		CancellationReasonSupersededByDirective: true,
		CancellationReasonCapabilityLoss:        true,
	}
	if len(want) != 3 {
		t.Fatalf("expected exactly three cancellation causes, got %d", len(want))
	}
	if CancellationReasonOperator == CancellationReasonSupersededByDirective {
		t.Error("Operator and SupersededByDirective must be distinct values")
	}
}
