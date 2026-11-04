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

// ─────────────────────────────────────────────────────────────────────────────
// A STAMP ATTRIBUTES ONE EDGE, NOT EVERY EDGE AFTER IT.
//
// The estop-actor stampers on FleetZone, SwarmadaConfig and the rollouts are
// failurePolicy Ignore (ADR-0046 decision 4): when the webhook server is unreachable the
// write lands unstamped. If the previous edge's stamp were still on the object, that edge
// would be sealed as the previous operator's act, as a user (ADR-0046 decision 5 forbids
// it). Each case seals one stamped edge, then lands the next edge as a skipped stamper
// would leave it, and requires the second entry to say it is unattributed.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/swarmada/swarmada/internal/audit"
)

// entriesOf returns every recorded entry of the given type, in order.
func entriesOf(spy *auditSpy, eventType string) []audit.Entry {
	var out []audit.Entry
	for _, e := range spy.entries {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

func requireUnattributed(t *testing.T, what string, e audit.Entry, scope string) {
	t.Helper()
	if e.Actor.Type != audit.ActorServiceAccount || e.Actor.Identity != unattributedPrefix+scope {
		t.Fatalf("%s actor = %s/%s, want %s/%s%s: the previous operator's stamp was attributed to an "+
			"edge nobody authenticated", what, e.Actor.Type, e.Actor.Identity,
			audit.ActorServiceAccount, unattributedPrefix, scope)
	}
}

func TestZoneEstop_SkippedStamperDoesNotInheritThePreviousOperator(t *testing.T) {
	ctx := context.Background()
	zone := zeZone("floor-1", "", nil, "true")
	zone.Annotations[annEstopActor] = "alice" // alice's trigger, stamped
	spy := &auditSpy{}
	r, c := newZEReconciler(t, &fakeEstopper{}, nil, zone, zeRobot("r-floor", "floor-1"))
	r.Audit = spy
	reconcileZE(t, r, "floor-1")
	if got := entriesOf(spy, audit.EventEstopTriggered); len(got) != 1 || got[0].Actor.Identity != "alice" {
		t.Fatalf("the stamped trigger was not attributed to alice: %+v", got)
	}

	// bob clears while the stamper is unreachable: the trigger is removed, nothing is stamped.
	z := zeGetZone(t, c, "floor-1")
	delete(z.Annotations, annEstopTriggered)
	if err := c.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")
	cleared := entriesOf(spy, audit.EventEstopCleared)
	if len(cleared) != 1 {
		t.Fatalf("ESTOP_CLEARED entries = %d, want 1", len(cleared))
	}
	requireUnattributed(t, "ESTOP_CLEARED", cleared[0], "zone-estop:floor-1")

	// carol re-triggers with the same value, stamper still unreachable.
	z = zeGetZone(t, c, "floor-1")
	if z.Annotations == nil {
		z.Annotations = map[string]string{}
	}
	z.Annotations[annEstopTriggered] = "true"
	if err := c.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")
	triggered := entriesOf(spy, audit.EventEstopTriggered)
	if len(triggered) != 2 {
		t.Fatalf("ESTOP_TRIGGERED entries = %d, want 2", len(triggered))
	}
	requireUnattributed(t, "second ESTOP_TRIGGERED", triggered[1], "zone-estop:floor-1")
}

func TestNamespaceEstop_SkippedStamperDoesNotInheritThePreviousOperator(t *testing.T) {
	ctx := context.Background()
	cfg := nsConfig("true", "")
	cfg.Annotations[annEstopActor] = "alice"
	spy := &auditSpy{}
	r, c := newNamespaceEstop(t, &fakeEstopper{}, cfg, zeRobot("r1", ""))
	r.Audit = spy
	reconcileNsEstop(t, r)
	if got := entriesOf(spy, audit.EventEstopTriggered); len(got) != 1 || got[0].Actor.Identity != "alice" {
		t.Fatalf("the stamped trigger was not attributed to alice: %+v", got)
	}

	got := nsGetConfig(t, c)
	delete(got.Annotations, annEstopTriggered)
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileNsEstop(t, r)
	cleared := entriesOf(spy, audit.EventEstopCleared)
	if len(cleared) != 1 {
		t.Fatalf("ESTOP_CLEARED entries = %d, want 1", len(cleared))
	}
	requireUnattributed(t, "ESTOP_CLEARED", cleared[0], "namespace-estop:"+zeNS)
}

// The rollout path shares the stamp. A resume that has been applied consumes it, so a
// later resume the stamper skipped reads as unattributed rather than as the earlier
// operator's.
func TestRolloutResume_AppliedResumeConsumesTheStamp(t *testing.T) {
	r, c := pausedRollout(t)
	spy := &auditSpy{}
	r.Audit = spy

	mr := getModelRollout(t, c)
	base := mr.DeepCopy()
	if mr.Annotations == nil {
		mr.Annotations = map[string]string{}
	}
	mr.Annotations[rolloutResumeAnnotation] = "r1 has a failed SSD"
	mr.Annotations[annEstopActor] = "carol" // as the stamper writes it on the same request
	if err := c.Patch(context.Background(), mr, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, r)

	resumed := entriesOf(spy, audit.EventRolloutResumed)
	if len(resumed) != 1 || resumed[0].Actor.Identity != "carol" {
		t.Fatalf("the stamped resume was not attributed to carol: %+v", resumed)
	}
	if v, ok := getModelRollout(t, c).Annotations[annEstopActor]; ok {
		t.Fatalf("estop-actor = %q is still on the rollout after its resume was sealed; the next "+
			"resume the stamper skips would be sealed as carol's", v)
	}
	if !strings.HasPrefix(estopActor(getModelRollout(t, c).Annotations, "modelrollout-controller").Identity,
		unattributedPrefix) {
		t.Fatal("with the stamp consumed, an unstamped resume must read as unattributed")
	}
}
