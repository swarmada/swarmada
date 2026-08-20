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

	"github.com/swarmada/swarmada/internal/audit"
)

// Operator attribution on estop and rollout-resume audit entries (ADR-0046).
//
// Before this, every estop entry named a synthetic actor derived from the scope
// (`zone-estop:warehouse-a`) with Type=service-account. The safety audit log therefore
// could not attribute an emergency stop to a person, and — worse for an auditor — an
// unattributable entry was indistinguishable from an attributed one, because both carried
// a plausible-looking service-account name.

func TestEstopActor_ResolvedUserIsTypedAsAUser(t *testing.T) {
	got := estopActor(map[string]string{annEstopActor: "alice"}, "zone-estop:warehouse-a")

	if got.Type != audit.ActorUser {
		t.Errorf("Type = %q, want %q — a resolved human must be recorded as a user",
			got.Type, audit.ActorUser)
	}
	if got.Identity != "alice" {
		t.Errorf("Identity = %q, want the authenticated username", got.Identity)
	}
}

func TestEstopActor_UnresolvedFallsBackWithoutClaimingAUser(t *testing.T) {
	got := estopActor(nil, "zone-estop:warehouse-a")

	// The invariant that matters most: an entry must never assert `user` when no user was
	// authenticated. A reader treating an unverified string as evidence is exactly the
	// failure a safety case cannot absorb.
	if got.Type == audit.ActorUser {
		t.Fatalf("Type = %q — the fallback claimed ActorUser with no authenticated identity",
			got.Type)
	}
	if got.Type != audit.ActorServiceAccount {
		t.Errorf("Type = %q, want %q", got.Type, audit.ActorServiceAccount)
	}
	if !strings.HasPrefix(got.Identity, unattributedPrefix) {
		t.Errorf("Identity = %q, want the %q marker — an unattributable entry must not read "+
			"like a configured service account", got.Identity, unattributedPrefix)
	}
	// The scope survives: which estop this was is still known, only who did it is not.
	if !strings.Contains(got.Identity, "zone-estop:warehouse-a") {
		t.Errorf("Identity = %q, want the scope retained after the marker", got.Identity)
	}
}

func TestEstopActor_EmptyAnnotationIsTreatedAsUnresolved(t *testing.T) {
	// An annotation present but empty is the shape a stale or stripped stamp leaves. It
	// must fall back, not record a nameless "user".
	got := estopActor(map[string]string{annEstopActor: ""}, "robot-estop:amr-1")
	if got.Type == audit.ActorUser {
		t.Fatal("an empty actor annotation was recorded as an authenticated user")
	}
	if !strings.HasPrefix(got.Identity, unattributedPrefix) {
		t.Errorf("Identity = %q, want the unattributed marker", got.Identity)
	}
}

// The identity written to Robot.status.estop.issuedBy/clearedBy must be the SAME string the
// audit entry carries, or the object and the log disagree about who stopped the fleet.
func TestEstopActorIdentity_MatchesTheAuditActor(t *testing.T) {
	for _, anns := range []map[string]string{
		{annEstopActor: "alice"},
		nil,
	} {
		a := estopActor(anns, "zone-estop:z")
		if got := estopActorIdentity(anns, "zone-estop:z"); got != a.Identity {
			t.Errorf("identity %q != audit actor identity %q", got, a.Identity)
		}
	}
}

// ── End to end through the controllers ───────────────────────────────────────
//
// The three cases the change has to get right: an authenticated trigger, an authenticated
// clear, and the fallback when no identity was stamped. Each asserts the ENVELOPE actor —
// never a Detail key, because scripts/specdiff.py's audit_detail_findings rejects
// envelope-carried identity duplicated into Detail (_ENVELOPE_FIELDS).

// findEntry returns the recorded entry of the given type, or fails.
func findEntry(t *testing.T, spy *auditSpy, eventType string) audit.Entry {
	t.Helper()
	for i := range spy.entries {
		if spy.entries[i].EventType == eventType {
			return spy.entries[i]
		}
	}
	t.Fatalf("no %s entry recorded; got %v", eventType, spy.typesRecorded())
	return audit.Entry{}
}

// assertNoIdentityInDetail guards the §9.6.5.1 envelope rule mechanically: a writer that
// helpfully copies the actor into Detail passes every behavioural test and fails the spec
// gate, so the tests should catch it first.
func assertNoIdentityInDetail(t *testing.T, e audit.Entry) {
	t.Helper()
	for _, k := range []string{"triggered_by", "cleared_by", "actor", "issued_by", "resumed_by"} {
		if v, ok := e.Detail[k]; ok {
			t.Errorf("Detail[%q] = %q duplicates envelope-carried identity "+
				"(scripts/specdiff.py _ENVELOPE_FIELDS)", k, v)
		}
	}
}

func TestZoneEstop_AuthenticatedTriggerIsAttributedToTheOperator(t *testing.T) {
	zone := zeZone("floor-1", "", nil, "sensor trip")
	zone.Annotations[annEstopActor] = "alice" // as the mutating webhook stamped it
	spy := &auditSpy{}
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil, zone, zeRobot("r-floor", "floor-1"))
	r.Audit = spy

	reconcileZE(t, r, "floor-1")

	e := findEntry(t, spy, audit.EventEstopTriggered)
	if e.Actor.Type != audit.ActorUser || e.Actor.Identity != "alice" {
		t.Errorf("ESTOP_TRIGGERED actor = %+v, want user/alice — the safety log must name "+
			"the person who stopped the fleet", e.Actor)
	}
	assertNoIdentityInDetail(t, e)
}

func TestZoneEstop_AuthenticatedClearIsAttributedToTheOperator(t *testing.T) {
	zone := zeZone("floor-1", "", nil, "sensor trip")
	zone.Annotations[annEstopActor] = "alice"
	spy := &auditSpy{}
	est := &fakeEstopper{}
	r, c := newZEReconciler(t, est, nil, zone, zeRobot("r-floor", "floor-1"))
	r.Audit = spy

	reconcileZE(t, r, "floor-1") // trigger, by alice

	// The clear is a SEPARATE authenticated act, and often a different person: whoever
	// removes the annotation is who the webhook restamps. Recording the trigger's actor
	// on the clear would misattribute the riskiest transition in the system — the one that
	// lets robots move again.
	z := zeGetZone(t, c, "floor-1")
	delete(z.Annotations, annEstopTriggered)
	z.Annotations[annEstopActor] = "bob"
	if err := c.Update(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")

	e := findEntry(t, spy, audit.EventEstopCleared)
	if e.Actor.Type != audit.ActorUser || e.Actor.Identity != "bob" {
		t.Errorf("ESTOP_CLEARED actor = %+v, want user/bob", e.Actor)
	}
	assertNoIdentityInDetail(t, e)
}

func TestZoneEstop_UnstampedTriggerIsRecordedUnattributedAndStillStops(t *testing.T) {
	// The fallback path: the webhook was unreachable (failurePolicy=Ignore), or the write
	// arrived by some route that carried no identity. The estop MUST still fan out — that
	// is the whole reason the stampers fail open — and the entry must say plainly that it
	// could not attribute the act.
	spy := &auditSpy{}
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", nil, "sensor trip"), // no estop-actor annotation
		zeRobot("r-floor", "floor-1"))
	r.Audit = spy

	reconcileZE(t, r, "floor-1")

	if got := est.names(); len(got) != 1 || got[0] != "r-floor" {
		t.Fatalf("estopped = %v — an unattributable estop was not carried out; identity "+
			"plumbing must never block a safe stop", got)
	}
	e := findEntry(t, spy, audit.EventEstopTriggered)
	if e.Actor.Type == audit.ActorUser {
		t.Errorf("actor claims %q with no authenticated identity", e.Actor.Type)
	}
	if !strings.HasPrefix(e.Actor.Identity, unattributedPrefix) {
		t.Errorf("actor identity = %q, want the %q marker so an auditor can tell an "+
			"unattributable entry from an attributed one", e.Actor.Identity, unattributedPrefix)
	}
}

func TestRolloutResume_IsAttributedToTheOperator(t *testing.T) {
	// ROLLOUT_RESUMED is an operator intent (ADR-0041), so it carries the operator. The
	// controller's other seals (install outcomes, the pause edge) are its own acts and
	// keep the service-account actor — asserted by the second half of this test.
	for _, tc := range []struct {
		name     string
		anns     map[string]string
		wantType audit.ActorType
		wantID   string
	}{
		{"stamped", map[string]string{annEstopActor: "carol"}, audit.ActorUser, "carol"},
		{"unstamped", nil, audit.ActorServiceAccount, unattributedPrefix + "firmwarerollout-controller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := estopActor(tc.anns, "firmwarerollout-controller")
			if got.Type != tc.wantType || got.Identity != tc.wantID {
				t.Errorf("actor = %+v, want %s/%s", got, tc.wantType, tc.wantID)
			}
		})
	}
}

// The two halves of ADR-0046 live in packages that cannot import each other (the
// dependency runs internal/controller -> internal/webhook only in the manager wiring), so
// the annotation name is spelled twice. A rename touching one side would silently degrade
// every estop entry to unattributed and break nothing else — this is the only thing that
// would notice.
func TestEstopActorAnnotation_SpellingIsPinned(t *testing.T) {
	const published = "swarmada.io/estop-actor"
	if annEstopActor != published {
		t.Errorf("controller-side annotation = %q, want %q (must match internal/webhook's "+
			"AnnEstopActor, which is what actually writes it)", annEstopActor, published)
	}
}
