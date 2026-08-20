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

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The mutating half of ADR-0046: carrying the authenticated operator from the admission
// request onto the object, so the controllers can attribute the audit entry later.

// ctxWithUser builds an admission context for an UPDATE whose old object carries oldAnns.
// user == "" models a request with no resolvable identity.
func ctxWithUser(t *testing.T, user string, oldAnns map[string]string) context.Context {
	t.Helper()
	old := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
		Name: "floor-1", Namespace: "warehouse-a", Annotations: oldAnns,
	}}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old object: %v", err)
	}
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo:  authnv1.UserInfo{Username: user},
			OldObject: runtime.RawExtension{Raw: raw},
		},
	})
}

// ctxCreate models a CREATE (no old object).
func ctxCreate(user string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			UserInfo:  authnv1.UserInfo{Username: user},
		},
	})
}

func zoneWith(anns map[string]string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
		Name: "floor-1", Namespace: "warehouse-a", Annotations: anns,
	}}
}

func TestStampEstopActor_TriggerStampsTheAuthenticatedUser(t *testing.T) {
	// alice adds the trigger annotation: the transition that fires an estop.
	z := zoneWith(map[string]string{estopTriggeredAnnotation: "sensor trip"})
	stampEstopActor(ctxWithUser(t, "alice", nil), z)

	if got := z.Annotations[AnnEstopActor]; got != "alice" {
		t.Errorf("estop-actor = %q, want alice", got)
	}
}

func TestStampEstopActor_ClearStampsTheClearingUser(t *testing.T) {
	// bob REMOVES the trigger annotation — the clear. The old object still has it, the
	// new one does not, and that removal is the transition to stamp. Getting this wrong
	// leaves the clear attributed to whoever triggered it, which is the single most
	// misleading entry the log could carry: the clear is what lets robots move again.
	z := zoneWith(map[string]string{AnnEstopActor: "alice"})
	stampEstopActor(ctxWithUser(t, "bob", map[string]string{
		estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "alice",
	}), z)

	if got := z.Annotations[AnnEstopActor]; got != "bob" {
		t.Errorf("estop-actor = %q after bob's clear, want bob", got)
	}
}

func TestStampEstopActor_UnrelatedUpdateDoesNotRestamp(t *testing.T) {
	// A label edit while an estop stands must not reattribute the standing estop to
	// whoever made that edit.
	same := map[string]string{estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "alice"}
	z := zoneWith(map[string]string{
		estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "alice",
	})
	stampEstopActor(ctxWithUser(t, "mallory", same), z)

	if got := z.Annotations[AnnEstopActor]; got != "alice" {
		t.Errorf("estop-actor = %q, want alice — an unrelated update reattributed the estop", got)
	}
}

func TestStampEstopActor_ReValuedTriggerRestamps(t *testing.T) {
	// A NEW trigger value re-fires the estop (zoneestop_controller), so it is a fresh act
	// by a possibly different person and must be re-attributed.
	z := zoneWith(map[string]string{
		estopTriggeredAnnotation: "second trip", AnnEstopActor: "alice",
	})
	stampEstopActor(ctxWithUser(t, "bob", map[string]string{
		estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "alice",
	}), z)

	if got := z.Annotations[AnnEstopActor]; got != "bob" {
		t.Errorf("estop-actor = %q on a re-fire, want bob", got)
	}
}

func TestStampEstopActor_ClientSuppliedValueCannotBeSpoofed(t *testing.T) {
	// A caller writing the annotation themselves must not be able to name someone else.
	// The webhook's stamp is an assertion about THIS request, so it overwrites.
	z := zoneWith(map[string]string{
		estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "ceo",
	})
	stampEstopActor(ctxWithUser(t, "mallory", nil), z)

	if got := z.Annotations[AnnEstopActor]; got != "mallory" {
		t.Errorf("estop-actor = %q, want mallory — a client-supplied actor survived", got)
	}
}

func TestStampEstopActor_UnresolvableIdentityStripsAStaleValue(t *testing.T) {
	// No username on the request. Leaving alice's stamp in place would attribute THIS
	// estop to her; an absent name is honest, a wrong one is not.
	z := zoneWith(map[string]string{
		estopTriggeredAnnotation: "second trip", AnnEstopActor: "alice",
	})
	stampEstopActor(ctxWithUser(t, "", map[string]string{
		estopTriggeredAnnotation: "sensor trip", AnnEstopActor: "alice",
	}), z)

	if got, ok := z.Annotations[AnnEstopActor]; ok {
		t.Errorf("estop-actor = %q, want it removed when no identity was resolvable", got)
	}
}

func TestStampEstopActor_NoAdmissionRequestIsNotFatal(t *testing.T) {
	// The bare-context path (a unit caller, or a runtime that did not populate it). It
	// must not panic and must not invent an actor — the estop still proceeds.
	z := zoneWith(map[string]string{estopTriggeredAnnotation: "sensor trip"})
	stampEstopActor(context.Background(), z)

	if got, ok := z.Annotations[AnnEstopActor]; ok {
		t.Errorf("estop-actor = %q, want none without an admission request", got)
	}
}

func TestStampEstopActor_CreateWithTriggerStamps(t *testing.T) {
	// A zone created with the trigger already on it is still an estop someone caused.
	z := zoneWith(map[string]string{estopTriggeredAnnotation: "sensor trip"})
	stampEstopActor(ctxCreate("alice"), z)

	if got := z.Annotations[AnnEstopActor]; got != "alice" {
		t.Errorf("estop-actor = %q on create-with-trigger, want alice", got)
	}
}

func TestStampEstopActor_CreateWithoutTriggerDoesNotStamp(t *testing.T) {
	// Ordinary zone creation is not an estop and must not be annotated as one.
	z := zoneWith(nil)
	stampEstopActor(ctxCreate("alice"), z)

	if got, ok := z.Annotations[AnnEstopActor]; ok {
		t.Errorf("estop-actor = %q on a plain create, want none", got)
	}
}

// The defaulters are thin shells; this pins that each is wired to the right transition key
// and rejects a foreign object type rather than silently doing nothing.
func TestEstopActorDefaulters_WireTheRightAnnotation(t *testing.T) {
	ctx := ctxCreate("alice")

	z := zoneWith(map[string]string{estopTriggeredAnnotation: "trip"})
	if err := (&FleetZoneEstopActorDefaulter{}).Default(ctx, z); err != nil {
		t.Fatalf("FleetZone defaulter: %v", err)
	}
	if z.Annotations[AnnEstopActor] != "alice" {
		t.Error("FleetZone defaulter did not stamp an estop trigger")
	}

	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "swarmada-config", Namespace: "warehouse-a",
		Annotations: map[string]string{estopTriggeredAnnotation: "trip"},
	}}
	if err := (&SwarmadaConfigEstopActorDefaulter{}).Default(ctx, cfg); err != nil {
		t.Fatalf("SwarmadaConfig defaulter: %v", err)
	}
	if cfg.Annotations[AnnEstopActor] != "alice" {
		t.Error("SwarmadaConfig defaulter did not stamp a namespace estop trigger")
	}

	fr := &fleetv1.FirmwareRollout{ObjectMeta: metav1.ObjectMeta{
		Name: "fw", Namespace: "warehouse-a",
		Annotations: map[string]string{annRolloutResume: "retry after fix"},
	}}
	if err := (&FirmwareRolloutResumeActorDefaulter{}).Default(ctx, fr); err != nil {
		t.Fatalf("FirmwareRollout defaulter: %v", err)
	}
	if fr.Annotations[AnnEstopActor] != "alice" {
		t.Error("FirmwareRollout defaulter did not stamp a resume")
	}

	mr := &fleetv1.ModelRollout{ObjectMeta: metav1.ObjectMeta{
		Name: "ml", Namespace: "warehouse-a",
		Annotations: map[string]string{annRolloutResume: "retry after fix"},
	}}
	if err := (&ModelRolloutResumeActorDefaulter{}).Default(ctx, mr); err != nil {
		t.Fatalf("ModelRollout defaulter: %v", err)
	}
	if mr.Annotations[AnnEstopActor] != "alice" {
		t.Error("ModelRollout defaulter did not stamp a resume")
	}

	// Wrong type in, error out — never a silent no-op.
	if err := (&FleetZoneEstopActorDefaulter{}).Default(ctx, &fleetv1.Robot{}); err == nil {
		t.Error("FleetZone defaulter accepted a Robot")
	}
}

// A rollout resume must not be stamped by the estop key, nor vice versa — they are
// separate transitions on separate resources.
func TestStampResumeActor_OnlyFiresOnAResumeTransition(t *testing.T) {
	fr := &fleetv1.FirmwareRollout{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{estopTriggeredAnnotation: "trip"},
	}}
	stampResumeActor(ctxCreate("alice"), fr)
	if _, ok := fr.Annotations[AnnEstopActor]; ok {
		t.Error("resume stamper fired on an estop-triggered annotation")
	}
}

// The webhook-side half of the same pin — see the controller-side twin.
func TestEstopActorAnnotation_SpellingIsPinned(t *testing.T) {
	if AnnEstopActor != "swarmada.io/estop-actor" {
		t.Errorf("AnnEstopActor = %q; the controller reads \"swarmada.io/estop-actor\"", AnnEstopActor)
	}
	if annRolloutResume != "swarmada.io/rollout-resume" {
		t.Errorf("annRolloutResume = %q; internal/controller writes \"swarmada.io/rollout-resume\"",
			annRolloutResume)
	}
}
