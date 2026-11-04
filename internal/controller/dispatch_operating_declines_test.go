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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The plane-side half of FA-R5, as a pure decision.
//
// The envtest beside this proves the reconciler acts on it end to end; these decide the
// memo's own contract, which has three edges the end-to-end run cannot reach cheaply: the
// TTL that lets a repaired adapter back in, the forget that stops a verdict outliving the
// attempt that produced it, and the per-action scoping that stops this becoming a global
// ban on a robot.

func declineAction(name, uid string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "ward"},
	}
}

func declineRobot(name string) fleetv1.Robot {
	return fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
}

func TestOperatingDecline_ADeclinedRobotIsNotOfferedTheSameActionAgain(t *testing.T) {
	var m operatingDeclineMemo
	action := declineAction("act-1", "u1")
	m.record(action, "amr-1", rejectionOperatingConstraintUnmet, []string{"camera"})

	kept, withheld := dropDeclinedCandidates(action, m.declined(action),
		[]fleetv1.Robot{declineRobot("amr-1"), declineRobot("amr-2")}, nil)

	if len(kept) != 1 || kept[0].Name != "amr-2" {
		t.Fatalf("eligible = %v, want only amr-2. FA-R5: the action returns to Pending and "+
			"the Scheduler tries the NEXT candidate — re-offering the assignment to the robot "+
			"that just declined it makes declining cost the adapter a round trip on every "+
			"requeue and the action everything", kept)
	}
	if len(withheld) != 1 || withheld[0].Robot != "amr-1" {
		t.Fatalf("withheld = %+v, want amr-1 named", withheld)
	}
	// Reported as FILTER 12, because it is the same rule reached from stronger evidence:
	// the filter believed the declaration, the adapter's refusal corrected it. One number
	// sends the operator to one place, and makes the action's own refusal
	// ZoneOperatingConstraintUnmet when every candidate has declined.
	if withheld[0].Filter != filterOperatingConstraint {
		t.Errorf("filter = %d, want %d", withheld[0].Filter, filterOperatingConstraint)
	}
	if !strings.Contains(withheld[0].Reason, "camera") {
		t.Errorf("reason %q does not name the subject the adapter refused", withheld[0].Reason)
	}
}

// SCOPED TO THE PAIR. The same robot is a perfectly good candidate for a different
// action; withholding it globally would be filter 11 with extra steps, which is the one
// mistake filter 12 is careful not to make.
func TestOperatingDecline_IsScopedToTheActionItWasRefusedFor(t *testing.T) {
	var m operatingDeclineMemo
	refused := declineAction("act-1", "u1")
	m.record(refused, "amr-1", rejectionOperatingConstraintUnmet, []string{"camera"})

	other := declineAction("act-2", "u2")
	kept, _ := dropDeclinedCandidates(other, m.declined(other),
		[]fleetv1.Robot{declineRobot("amr-1")}, nil)
	if len(kept) != 1 {
		t.Fatal("a robot that declined ONE action was withheld from another. The refusal is " +
			"about the rules in force where that action sends it, not about the robot")
	}
}

// A recreated object of the same name is a NEW question: the UID is in the key, so a
// verdict about a deleted action cannot silently govern its replacement.
func TestOperatingDecline_ARecreatedActionStartsClean(t *testing.T) {
	var m operatingDeclineMemo
	m.record(declineAction("act-1", "u1"), "amr-1", rejectionOperatingConstraintUnmet, []string{"camera"})

	recreated := declineAction("act-1", "u2")
	if got := m.declined(recreated); got != nil {
		t.Fatalf("a recreated action inherited %v from the object it replaced", got)
	}
}

// FORGET ON ACCEPTANCE. Once a candidate takes the work, "who has already refused this"
// is a closed question, and an action later reassigned deserves a fresh look at the whole
// fleet rather than a verdict inherited from a previous attempt against a zone rule that
// may since have changed.
func TestOperatingDecline_AcceptanceClearsTheMemo(t *testing.T) {
	var m operatingDeclineMemo
	action := declineAction("act-1", "u1")
	m.record(action, "amr-1", rejectionOperatingConstraintUnmet, []string{"camera"})
	m.forget(action)

	if got := m.declined(action); got != nil {
		t.Fatalf("the memo survived an acceptance: %v", got)
	}
}

// THE TTL IS WHAT LETS A REPAIRED ADAPTER BACK IN. A vendor ships a build that honours
// the subject, or the operator withdraws the rule; neither notifies this map, so the
// entry ages out instead — which is also what stops a long-lived controller accumulating
// an entry for every pair it has ever seen.
func TestOperatingDecline_AnAgedEntryStopsWithholding(t *testing.T) {
	var m operatingDeclineMemo
	action := declineAction("act-1", "u1")
	m.record(action, "amr-1", rejectionOperatingConstraintUnmet, []string{"camera"})

	// Reach in and age the entry rather than sleeping: the property under test is the
	// cutoff, not the clock.
	m.mu.Lock()
	e := m.by[declineKey(action)]["amr-1"]
	e.at = time.Now().Add(-operatingDeclineTTL - time.Minute)
	m.by[declineKey(action)]["amr-1"] = e
	m.mu.Unlock()

	if got := m.declined(action); got != nil {
		t.Fatalf("an entry older than the TTL still withheld a robot: %v. A rebuilt adapter "+
			"that no operator action can announce must be retried eventually, or this cache "+
			"becomes a permanent ban held in one process's memory", got)
	}
	// And the map does not keep the dead entry.
	m.mu.Lock()
	remaining := len(m.by)
	m.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d action key(s) retained after every entry aged out", remaining)
	}
}

// A DECLINE THAT NAMES NOTHING IS STILL RECORDED. FA-R5 requires the names and an adapter
// that omits them is non-conformant — but the plane must not answer a malformed refusal by
// re-offering the assignment for ever.
func TestOperatingDecline_ADeclineNamingNothingStillWithholds(t *testing.T) {
	var m operatingDeclineMemo
	action := declineAction("act-1", "u1")
	m.record(action, "amr-1", rejectionOperatingConstraintUnmet, nil)

	kept, withheld := dropDeclinedCandidates(action, m.declined(action),
		[]fleetv1.Robot{declineRobot("amr-1")}, nil)
	if len(kept) != 0 {
		t.Fatal("a decline that named no subject was re-offered; a non-conformant refusal is " +
			"still a refusal, and the loop it would cause is the plane's problem, not the " +
			"adapter's")
	}
	if !strings.Contains(withheld[0].Reason, "does not satisfy FA-R5") {
		t.Errorf("reason %q does not record that the decline named nothing, which is the "+
			"adapter's conformance failure and the operator's only clue to it",
			withheld[0].Reason)
	}
}

// The zero value is usable and an empty memo changes nothing, so every existing
// construction of the reconciler behaves exactly as it did.
func TestOperatingDecline_AnEmptyMemoWithholdsNobody(t *testing.T) {
	var m operatingDeclineMemo
	action := declineAction("act-1", "u1")
	in := []fleetv1.Robot{declineRobot("amr-1"), declineRobot("amr-2")}
	kept, withheld := dropDeclinedCandidates(action, m.declined(action), in, nil)
	if len(kept) != 2 || len(withheld) != 0 {
		t.Fatalf("an empty memo changed the candidate set: kept=%v withheld=%+v", kept, withheld)
	}
}
