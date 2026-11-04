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
	"github.com/swarmada/swarmada/internal/audit"
)

// Resolving the audited actor for an estop or a rollout resume (ADR-0046).
//
// The mutating webhooks stamp swarmada.io/estop-actor from the admission request's
// UserInfo at the moment the operator writes the annotation. By the time a controller
// reconciles, that request is long gone — this is the only surviving record of who acted.

const (
	// annEstopActor is written by the mutating webhooks in internal/webhook
	// (AnnEstopActor there) and read here. A controller never writes it; it removes it
	// once the entry it attributes is sealed (consumeEstopActor).
	annEstopActor = "swarmada.io/estop-actor"

	// unattributedPrefix marks an actor the control plane could NOT resolve to a person.
	//
	// It is deliberately not a plausible identity. The previous synthetic actors
	// (`zone-estop:<zone>`) read exactly like a service account someone had configured,
	// so an auditor reading the log had no way to tell an attributed entry from an
	// unattributable one. A reader who sees `unattributed:` knows the identity is absent
	// rather than being quietly handed a name that was never checked.
	//
	// The scope suffix is retained after the prefix because it still says WHICH estop
	// this was, which is the part that was never in doubt.
	unattributedPrefix = "unattributed:"
)

// estopActor resolves the audit actor for an estop or resume event.
//
// The annotations come from the carrier object the controller already fetched.
// `scope` is the pre-existing synthetic descriptor (e.g. "zone-estop:warehouse-a") and is
// used only for the unattributed fallback, where it preserves which estop is being
// described.
//
// Two shapes, and the distinction is load-bearing for a safety case:
//
//	ActorUser + "alice"                     — a human the API server authenticated
//	ActorServiceAccount + "unattributed:…"  — nobody was resolved; the entry says so
//
// The fallback MUST NOT claim ActorUser. An entry that says `user` when no user was
// authenticated is worse than one that admits it does not know: it invites a reader to
// treat an unverified string as evidence.
//
// This function never fails. An estop is a safe stop, and a safe stop is always honoured —
// unresolvable identity degrades the record, never the response (RFC-0001 #safety-emergency-stop-protocol).
func estopActor(annotations map[string]string, scope string) audit.Actor {
	if user := annotations[annEstopActor]; user != "" {
		return audit.Actor{Type: audit.ActorUser, Identity: user}
	}
	return audit.Actor{Type: audit.ActorServiceAccount, Identity: unattributedPrefix + scope}
}

// consumeEstopActor removes the operator stamp once the entry it attributes has been
// sealed. Called in the same patch that records the edge as processed (the estop marker
// set or dropped, or the rollout-resume marker set), so it costs no extra write.
//
// Without it a stamp outlives the edge it was written for. The stampers on FleetZone,
// SwarmadaConfig and the two rollouts are failurePolicy Ignore (ADR-0046 decision 4), so
// when one is skipped on the NEXT edge the previous operator's name is still on the
// object, and estopActor would seal that edge as theirs, as a user, which ADR-0046
// decision 5 forbids. Once consumed, a skipped stamp reads as absent and the edge is
// sealed unattributed. The one cost runs the safe way: a stamp written while an earlier
// edge is still being fanned out is removed with the earlier one, so that later edge is
// recorded unattributed, never as someone else's.
func consumeEstopActor(annotations map[string]string) {
	delete(annotations, annEstopActor)
}

// estopActorIdentity is the identity string for the wire/state fields that carry an
// operator name (Robot.status.estop.issuedBy / clearedBy). It mirrors estopActor's
// resolution so the object field and the audit entry can never disagree about who acted.
func estopActorIdentity(annotations map[string]string, scope string) string {
	return estopActor(annotations, scope).Identity
}
