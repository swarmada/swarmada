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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FA-R5's CONTROL-PLANE half: "the action returns to `Pending` and the Scheduler tries
// the NEXT candidate".
//
// # The sentence is an obligation on the plane, not only on the adapter
//
// FA-R5 says declining costs the adapter nothing. Without this, it costs it a round trip
// on every requeue and costs the ACTION everything: the assignment is released to
// Pending, the scheduler re-ranks the same candidates by the same battery-descending key
// (internal/scheduler, SelectRobot), offers the action to the robot that just declined
// it, and receives the same decline — until the deadline, with a robot that CAN serve it
// standing in the same zone and never being asked. An action that livelocks on a refusal
// is not a refusal that costs nothing.
//
// MEASURED before this existed: two candidates in the same leaf, one whose adapter cannot
// honour the rule, and the assignments pushed were [amr-1 amr-1].
//
// # Why the memory is scoped to THIS rejection and not to every rejection
//
// The other five rejection values are facts about a MOMENT. A busy robot becomes free, an
// unreachable adapter reconnects, a stale fencing token is superseded by the next
// generation; re-offering after a backoff is the right behaviour for all of them, and
// that behaviour is deliberately left alone here.
//
// OPERATING_CONSTRAINT_UNMET is not like them. It is a STANDING property of this (robot,
// action) pair for as long as the zone rule stands and the adapter is the build it is:
// the adapter has just told the plane, on the wire, that it cannot honour a subject in
// force where that robot is. Nothing about waiting changes it. Re-offering is the one
// case where the retry is knowably pointless before it is made.
//
// # It is a CACHE, not state, and that is what makes it safe to hold in memory
//
// Losing it — a controller restart, a leader election — costs exactly one wasted round
// trip, and the adapter declines again and it is relearned. It can therefore live in the
// process rather than on the object, which keeps a scheduling heuristic out of the API
// surface where it would become a compatibility commitment. It is NOT a safety gate:
// candidate filter 12 is, and it runs first and is unaffected by any of this.
//
// # The TTL exists so a repaired adapter is retried without a restart
//
// A vendor ships a build that honours the subject, or the operator withdraws the rule.
// Neither event notifies this map. The entry ages out instead, and the robot is offered
// the work again — which is also what stops a long-lived controller accumulating an entry
// per (action, robot) pair it has ever seen.
const operatingDeclineTTL = 15 * time.Minute

// operatingDeclineMemo remembers which robots declined which action under
// ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET, and which subjects each named.
//
// Keyed by action and then robot, because the fact is about the PAIR: the same robot is a
// perfectly good candidate for an action targeting a zone that restricts nothing, and
// withholding it globally would be filter 11 with extra steps — the same mistake filter
// 12 is careful not to make.
type operatingDeclineMemo struct {
	mu sync.Mutex
	by map[string]map[string]operatingDecline
}

// operatingDecline is one adapter's refusal, as the plane received it.
//
// rejection is the wire value, carried because the memo now holds TWO standing refusals
// and they report differently. Keeping one memo with a reason beside each entry, rather
// than a second map per value, is the same argument the latch in zone_occupant_report.go
// makes: two mechanisms with one job are two things to keep in step.
type operatingDecline struct {
	rejection string
	subjects  []string
	at        time.Time
}

// record notes that robot declined action naming subjects. A decline naming nothing is
// still recorded: FA-R5 requires the names and an adapter that omits them is
// non-conformant, but the plane must not respond to a malformed refusal by re-offering
// the assignment for ever.
func (m *operatingDeclineMemo) record(
	action *fleetv1.FleetAction, robot, rejection string, subjects []string,
) {
	if m == nil || action == nil || robot == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]map[string]operatingDecline{}
	}
	key := declineKey(action)
	if m.by[key] == nil {
		m.by[key] = map[string]operatingDecline{}
	}
	sorted := append([]string(nil), subjects...)
	sort.Strings(sorted)
	m.by[key][robot] = operatingDecline{rejection: rejection, subjects: sorted, at: time.Now()}
}

// declined returns the live declines for one action, dropping any that have aged out.
func (m *operatingDeclineMemo) declined(action *fleetv1.FleetAction) map[string]operatingDecline {
	if m == nil || action == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := declineKey(action)
	seen := m.by[key]
	if len(seen) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-operatingDeclineTTL)
	live := make(map[string]operatingDecline, len(seen))
	for robot, d := range seen {
		if d.at.Before(cutoff) {
			delete(seen, robot)
			continue
		}
		live[robot] = d
	}
	if len(seen) == 0 {
		delete(m.by, key)
	}
	if len(live) == 0 {
		return nil
	}
	return live
}

// forget drops every decline recorded for an action. Called once the action is committed
// to a robot: the question this memo answers — "who has already refused this assignment"
// — is closed the moment one candidate accepts it, and an action that is later reassigned
// deserves a fresh look at the whole fleet rather than a verdict inherited from a
// previous attempt against a possibly-different zone rule.
func (m *operatingDeclineMemo) forget(action *fleetv1.FleetAction) {
	if m == nil || action == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.by, declineKey(action))
}

// declineKey identifies an action across reconciles. The UID is included so a deleted and
// recreated action of the same name starts clean — a new object is a new question.
func declineKey(action *fleetv1.FleetAction) string {
	return action.Namespace + "/" + action.Name + "/" + string(action.UID)
}

// dropDeclinedCandidates removes from the eligible set every robot that has already
// declined THIS action on an operating constraint, and reports each as a filter-12
// exclusion.
//
// REPORTED AS FILTER 12, and that is not a convenience. It is the same rule: the robot
// cannot honour an operating subject in force where it stands. Filter 12 reached that
// conclusion from the robot's DECLARATION; this reached it from the adapter's own refusal
// on the wire, which is strictly better evidence — a declaration is a claim, a decline is
// something the adapter did (ADR-0058 §12). Reporting them under one number sends the
// operator to one place, and makes the action's refusal
// ZoneOperatingConstraintUnmet — "no robot here can honour the rule" — when every
// candidate has declined, which is the true sentence and already the specified one.
//
// It runs OUTSIDE the numbered candidate-filter chain on purpose: the chain is RFC-0001's
// roster, and this is a scheduling memo held in process, not a filter a conforming plane
// must implement. Placing it here keeps dispatch_filters.go describing exactly the ten
// filters the specification names.
func dropDeclinedCandidates(
	action *fleetv1.FleetAction, declined map[string]operatingDecline,
	eligible []fleetv1.Robot, withheld []dispatchExclusion,
) ([]fleetv1.Robot, []dispatchExclusion) {
	if len(declined) == 0 {
		return eligible, withheld
	}
	kept := make([]fleetv1.Robot, 0, len(eligible))
	for i := range eligible {
		d, refused := declined[eligible[i].Name]
		if !refused {
			kept = append(kept, eligible[i])
			continue
		}
		if d.rejection == rejectionInvalidAction {
			// A DIFFERENT REFUSAL AND A DIFFERENT SENTENCE. The operating-constraint decline
			// below is about a zone rule the adapter cannot honour. This one is about the
			// ACTION: the adapter says it cannot parse or cannot serve this type. Reporting
			// them under one phrase would send an operator to the zone when the answer is in
			// the FleetAction, which is the one distinction this exclusion exists to make.
			withheld = append(withheld, dispatchExclusion{
				Robot:  eligible[i].Name,
				Filter: filterAdapterRefusedAction,
				Reason: "robot's adapter REFUSED this assignment as INVALID_ACTION — it cannot " +
					"parse spec.payload or does not implement spec.type. The wire value does not " +
					"say which, so the plane does not guess: it stops re-offering THIS " +
					"assignment to THIS adapter and tries the next candidate. Nothing about " +
					"waiting changes either half",
			})
			continue
		}
		named := "naming no subject, which does not satisfy FA-R5"
		if len(d.subjects) > 0 {
			named = "naming " + strings.Join(d.subjects, ", ")
		}
		withheld = append(withheld, dispatchExclusion{
			Robot:  eligible[i].Name,
			Filter: filterOperatingConstraint,
			Reason: fmt.Sprintf("robot's adapter DECLINED this assignment %s; its declaration "+
				"said it honours the subject and the adapter, asked to, did not. The decline is "+
				"the stronger evidence — a declaration is a claim, a decline is something the "+
				"adapter did — so the action goes to the next candidate rather than back to "+
				"this one", named),
		})
	}
	return kept, withheld
}

// everyCandidateRefusedTheAction reports whether the ONLY reason nothing was dispatched
// is that every candidate's adapter refused this assignment as INVALID_ACTION.
//
// # This is the aggregate the plane CAN trust, from a value it cannot trust individually
//
// `INVALID_ACTION` conflates "this adapter does not implement this type" with "this
// payload is unparseable" under one wire value, so no single refusal tells the plane
// which. But if EVERY eligible candidate refuses with it, the remaining explanation that
// fits is the action — and that is a conclusion drawn from the pattern rather than from
// the enum, which is why it is available here and not at the refusal.
//
// IT DOES NOT MAKE THE ACTION TERMINAL, and the restraint is the point. A conforming
// adapter deployed tomorrow makes the same action dispatchable, exactly as it does for
// ZoneOperatingConstraintUnmet, so the action waits and fails on its deadline like any
// other unsatisfied Pending. What changes is that the operator is told which of the two
// questions to go and answer.
func everyCandidateRefusedTheAction(withheld []dispatchExclusion, survivors int) bool {
	if survivors > 0 || len(withheld) == 0 {
		return false
	}
	for _, w := range withheld {
		if w.Filter != filterAdapterRefusedAction {
			return false
		}
	}
	return true
}
