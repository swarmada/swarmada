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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FleetZone.spec.restrictions — the schema half of ADR-0058.
//
// EVERY RULE HERE IS CEL ON THE GENERATED CRD, SO EVERY TEST HERE IS AN ENVTEST.
// A CEL rule exercised against the fake client is a rule that has never run: the
// fake performs no admission at all, so a typo in the rule, a wrong CEL path, a
// marker on the wrong type, or a rule that CRD regeneration silently dropped all
// behave exactly like a correct rule — the object is accepted either way and
// nothing ever says so. fleettask_cel_envtest_test.go is the precedent in this
// package: it was written for exactly this reason, against a CEL rule the
// specification claimed was enforced and the fake client could never exercise.
//
// The failure these tests stand between a user and is the whole point of ADR-0058:
// a restriction an operator wrote in good faith, accepted by the API server, and
// silently meaningless. A zone restriction is not a safety control — the ADR says
// so plainly — but a restriction the schema accepts and no rule constrains is worse
// than none, because it is believed.

// zoneWithRestrictions builds a minimal FleetZone carrying the given restriction block.
func zoneWithRestrictions(ns, name string, r *fleetv1.ZoneRestrictions) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetZoneSpec{DisplayName: name, Restrictions: r},
	}
}

// zoneKey is the NamespacedName of a zone in the test namespace.
func zoneKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: ns}
}

// ---------------------------------------------------------------------------
// Compatibility: a FleetZone with no restrictions block is untouched (ADR-0058,
// "spec.restrictions is additive and optional").
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneWithoutRestrictionsStillApplies(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// The FleetZone out of config/samples/demo_a.yaml, verbatim in shape: no
	// restrictions block anywhere. Every FleetZone written before ADR-0058 looks
	// like this, and "absence means the zone states no restriction" is the entire
	// compatibility story.
	const existing = `
apiVersion: swarmada.io/v1
kind: FleetZone
metadata:
  name: warehouse-a
spec:
  displayName: "Receiving and staging area — east wing"
  maxConcurrentRobots: 0
  physicalBounds:
    floor: 0
    polygon:
      - { x: -5.0, "y": -5.0 }
      - { x: 25.0, "y": -5.0 }
      - { x: 25.0, "y": 20.0 }
      - { x: -5.0, "y": 20.0 }
  waypoints:
    - name: receiving-dock-1
      x: 5.1
      "y": 2.1
      yaw: 3.14
  sharedResources:
    - name: main-aisle
      type: Corridor
      capacity: 1
      reservationPolicy: FIFO
`
	var fz fleetv1.FleetZone
	if err := yaml.UnmarshalStrict([]byte(existing), &fz); err != nil {
		t.Fatalf("the pre-ADR-0058 manifest no longer decodes into FleetZone: %v", err)
	}
	fz.Namespace = ns

	if err := envK8s.Create(ctx, &fz); err != nil {
		t.Fatalf("the API server rejected an existing FleetZone that carries no "+
			"restrictions block — spec.restrictions was supposed to be additive and "+
			"optional, and this is the compatibility claim ADR-0058 makes: %v", err)
	}

	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "warehouse-a"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Spec.Restrictions != nil {
		t.Errorf("a zone that stated no restriction came back with a restrictions "+
			"block: %#v — absence must stay absence; nothing may default a rule into "+
			"existence, because a defaulted rule is a rule nobody wrote", got.Spec.Restrictions)
	}
	// Everything that was there before must still be there.
	if got.Spec.DisplayName == "" || got.Spec.PhysicalBounds == nil ||
		len(got.Spec.Waypoints) != 1 || len(got.Spec.SharedResources) != 1 {
		t.Errorf("the pre-existing spec did not round-trip intact: %#v", got.Spec)
	}
}

// ---------------------------------------------------------------------------
// ADR-0058 §2 — there is no Allowed mode, at any axis.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneRestrictionsNoAllowedMode(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	const allowed = fleetv1.ZoneRestrictionMode("Allowed")

	for _, tc := range []struct {
		name string
		r    *fleetv1.ZoneRestrictions
	}{
		{"entry", &fleetv1.ZoneRestrictions{
			Entry: &fleetv1.ZoneEntryRestriction{Mode: allowed},
		}},
		{"capability", &fleetv1.ZoneRestrictions{
			Capabilities: []fleetv1.ZoneCapabilityRestriction{{Name: "clean.mop", Mode: allowed}},
		}},
		{"operating", &fleetv1.ZoneRestrictions{
			Operating: []fleetv1.ZoneOperatingRestriction{{Subject: "camera", Mode: allowed}},
		}},
	} {
		err := envK8s.Create(ctx, zoneWithRestrictions(ns, "allowed-"+tc.name, tc.r))
		if err == nil {
			t.Errorf("%s: the API server accepted mode: Allowed. ADR-0058 §2 removes the "+
				"permissive mode at the SCHEMA so that a hole is UNWRITABLE, not merely "+
				"rejected at admission. An accepted Allowed would be written in good faith, "+
				"be silently inert, and be believed", tc.name)
		}
	}
}

// ---------------------------------------------------------------------------
// ADR-0058 §1, §2 — entry.mode must be Denied; presence is the denial.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneEntryModeMustBeDenied(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	for _, bad := range []fleetv1.ZoneRestrictionMode{
		fleetv1.ZoneRestrictionLimited,
		fleetv1.ZoneRestrictionRequired,
	} {
		err := envK8s.Create(ctx, zoneWithRestrictions(ns, "entry-"+strings.ToLower(string(bad)),
			&fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{Mode: bad}}))
		if err == nil {
			t.Errorf("the API server accepted entry.mode: %s. There is no bound and no "+
				"requirement that makes sense of \"may a robot be here at all\"; "+
				"Limited and Required are valid only on operating rules", bad)
			continue
		}
		if !strings.Contains(err.Error(), "must be Denied") {
			t.Errorf("entry.mode: %s was rejected, but not by the expected rule: %v", bad, err)
		}
	}

	// Denied must still be accepted, and occupantAction must default to Report —
	// a validation test that only proves a rejection cannot tell a correct rule
	// from one that rejects everything.
	z := zoneWithRestrictions(ns, "entry-denied", &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{
			Mode:   fleetv1.ZoneRestrictionDenied,
			Reason: "outdoors behind the fence; no robot access",
		},
	})
	if err := envK8s.Create(ctx, z); err != nil {
		t.Fatalf("the rule rejected the one valid entry restriction: %v", err)
	}
	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "entry-denied"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Spec.Restrictions == nil || got.Spec.Restrictions.Entry == nil {
		t.Fatalf("the entry restriction did not survive the write — the API server pruned "+
			"spec.restrictions, which is what an unregenerated CRD does: %#v", got.Spec.Restrictions)
	}
	if got.Spec.Restrictions.Entry.OccupantAction != fleetv1.ZoneOccupantReport {
		t.Errorf("entry.occupantAction defaulted to %q, want Report. ADR-0058 §10 makes "+
			"Report the default because a control plane that starts MOVING ROBOTS in "+
			"response to a spec edit is a new autonomous motion path",
			got.Spec.Restrictions.Entry.OccupantAction)
	}
	if got.Spec.Restrictions.Entry.Reason == "" {
		t.Error("entry.reason did not round-trip; it is what the ZoneEntryDenied refusal carries")
	}
}

// ---------------------------------------------------------------------------
// ADR-0058 §8 — axis 2 carries Denied only. No max, no allowed, no Limited.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneCapabilityModeMustBeDenied(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	for _, bad := range []fleetv1.ZoneRestrictionMode{
		fleetv1.ZoneRestrictionLimited,
		fleetv1.ZoneRestrictionRequired,
	} {
		err := envK8s.Create(ctx, zoneWithRestrictions(ns, "cap-"+strings.ToLower(string(bad)),
			&fleetv1.ZoneRestrictions{Capabilities: []fleetv1.ZoneCapabilityRestriction{
				{Name: "clean.mop", Mode: bad},
			}}))
		if err == nil {
			t.Errorf("the API server accepted capabilities[].mode: %s. ADR-0058 §8: "+
				"\"may clean here, but at 0.4 m/s\" is TWO RULES, one per axis — a bound "+
				"under a capability name would duplicate the axis-3 subject vocabulary "+
				"in a second place with a second set of units", bad)
			continue
		}
		if !strings.Contains(err.Error(), "must be Denied") {
			t.Errorf("capabilities[].mode: %s was rejected, but not by the expected rule: %v", bad, err)
		}
	}

	// Denied is accepted, including the underscored capability form that
	// FleetAction.spec.requiredCapabilities actually carries today.
	ok := zoneWithRestrictions(ns, "cap-denied", &fleetv1.ZoneRestrictions{
		Capabilities: []fleetv1.ZoneCapabilityRestriction{
			{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied, Reason: "carpet that must not get wet"},
			{Name: "clean_area", Mode: fleetv1.ZoneRestrictionDenied},
		},
	})
	if err := envK8s.Create(ctx, ok); err != nil {
		t.Fatalf("the rule rejected valid capability denials — note clean_area: a pattern "+
			"this axis applied but requiredCapabilities did not would make some capability "+
			"impossible to deny: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ADR-0058 §7 — the subject vocabulary: closed core, namespaced extension, and
// NOT the capability vocabulary.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneOperatingSubjectVocabulary(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// Every one of the normative core seven must be accepted, bare.
	if len(fleetv1.ZoneCoreOperatingSubjects) != 7 {
		t.Fatalf("the core vocabulary has %d subjects, not seven: %v",
			len(fleetv1.ZoneCoreOperatingSubjects), fleetv1.ZoneCoreOperatingSubjects)
	}
	for i, subj := range fleetv1.ZoneCoreOperatingSubjects {
		z := zoneWithRestrictions(ns, "core-"+subj, &fleetv1.ZoneRestrictions{
			Operating: []fleetv1.ZoneOperatingRestriction{
				{Subject: subj, Mode: fleetv1.ZoneRestrictionDenied},
			},
		})
		if err := envK8s.Create(ctx, z); err != nil {
			t.Errorf("core subject %d (%q) was rejected; conformance covers exactly "+
				"these seven: %v", i, subj, err)
		}
	}

	// A namespaced extension subject must be accepted — the set is closed AT THE
	// CORE, not closed.
	if err := envK8s.Create(ctx, zoneWithRestrictions(ns, "ext-ok", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "example.com/leaf-blower", Mode: fleetv1.ZoneRestrictionDenied},
		},
	})); err != nil {
		t.Errorf("a namespaced extension subject was rejected; the vocabulary is closed "+
			"at the core and namespaced at the edge, not an Enum: %v", err)
	}

	// A bare non-core subject must be REJECTED, and the message must point at the
	// extension form rather than leaving the operator stuck.
	for _, bad := range []string{"lidar", "sensor.microphone", "vacuum"} {
		err := envK8s.Create(ctx, zoneWithRestrictions(ns, "bare-"+strings.ReplaceAll(bad, ".", "-"),
			&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
				{Subject: bad, Mode: fleetv1.ZoneRestrictionDenied},
			}}))
		if err == nil {
			t.Errorf("the API server accepted bare subject %q. The core is CLOSED: an "+
				"unnamespaced subject outside the seven is how a site-policy vocabulary "+
				"drifts per vendor, which is the defect ADR-0058 exists to close", bad)
			continue
		}
		if !strings.Contains(err.Error(), "core seven") {
			t.Errorf("bare subject %q was rejected, but not by the expected rule: %v", bad, err)
		}
	}

	// Shape rules on the extension form: uppercase and a bare leading slash are not
	// DNS-subdomain-prefixed lowercase identifiers.
	// Distinct object names: reusing one name would make every iteration after the
	// first fail with AlreadyExists and read as a pass.
	for i, bad := range []string{"Example.com/Leaf", "/leaf-blower", "example.com/", "camera "} {
		if err := envK8s.Create(ctx, zoneWithRestrictions(ns, fmt.Sprintf("shape-%d", i), &fleetv1.ZoneRestrictions{
			Operating: []fleetv1.ZoneOperatingRestriction{
				{Subject: bad, Mode: fleetv1.ZoneRestrictionDenied},
			},
		})); err == nil {
			t.Errorf("the API server accepted malformed subject %q; the extension form is "+
				"'<dns-subdomain>/<name>', lowercase", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// The amendment's cross-field rules on an operating rule: max and allowed are
// meaningful only under Limited, and a Limited rule sets exactly one of them.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneOperatingLimitedExactlyOneParameter(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	type tc struct {
		name   string
		rule   fleetv1.ZoneOperatingRestriction
		accept bool
		expect string
	}
	for _, c := range []tc{
		{
			name:   "limited-max-only",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "0.4"},
			accept: true,
		},
		{
			name: "limited-allowed-only",
			rule: fleetv1.ZoneOperatingRestriction{
				Subject: "chemical", Mode: fleetv1.ZoneRestrictionLimited,
				Allowed: []string{"quat-ammonium-7"},
			},
			accept: true,
		},
		{
			name: "limited-both",
			rule: fleetv1.ZoneOperatingRestriction{
				Subject: "chemical", Mode: fleetv1.ZoneRestrictionLimited,
				Max: "3", Allowed: []string{"quat-ammonium-7"},
			},
			expect: "exactly one of max or allowed",
		},
		{
			name:   "limited-neither",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited},
			expect: "exactly one of max or allowed",
		},
		{
			name:   "denied-with-max",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "speed", Mode: fleetv1.ZoneRestrictionDenied, Max: "0.4"},
			expect: "only when mode is Limited",
		},
		{
			name: "required-with-allowed",
			rule: fleetv1.ZoneOperatingRestriction{
				Subject: "chemical", Mode: fleetv1.ZoneRestrictionRequired,
				Allowed: []string{"quat-ammonium-7"},
			},
			expect: "only when mode is Limited",
		},
		{
			name:   "denied-bare",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied},
			accept: true,
		},
		{
			name:   "required-bare",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "camera", Mode: fleetv1.ZoneRestrictionRequired},
			accept: true,
		},
		{
			// The unit is a property of the subject, not of the value: no suffix, no
			// exponent. "400m" must not be mistakable for 400 metres or 0.4.
			name:   "max-with-quantity-suffix",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "400m"},
			expect: "",
		},
		{
			name:   "max-negative",
			rule:   fleetv1.ZoneOperatingRestriction{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "-1"},
			expect: "",
		},
	} {
		err := envK8s.Create(ctx, zoneWithRestrictions(ns, "op-"+c.name,
			&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{c.rule}}))
		switch {
		case c.accept && err != nil:
			t.Errorf("%s: the rule rejected a valid operating restriction: %v", c.name, err)
		case !c.accept && err == nil:
			t.Errorf("%s: the API server accepted %+v. max bounds a scalar and allowed "+
				"enumerates a set; a subject is one or the other, and a Denied or Required "+
				"rule carries neither", c.name, c.rule)
		case !c.accept && c.expect != "" && !strings.Contains(err.Error(), c.expect):
			t.Errorf("%s: rejected, but not by the expected rule (want %q): %v", c.name, c.expect, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The map keys landed: +listType=map on capabilities (name) and operating
// (subject). Without them server-side apply replaces the whole list and two
// operators editing different rules clobber each other — the reason ADR-0058
// rejected one flat rules[] list.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneRestrictionListsAreMaps(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	if err := envK8s.Create(ctx, zoneWithRestrictions(ns, "dup-subject", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied},
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionRequired},
		},
	})); err == nil {
		t.Error("the API server accepted two operating rules on subject 'camera'. " +
			"operating is +listType=map +listMapKey=subject, so a duplicate key is a " +
			"rejected object, not a last-one-wins merge")
	}

	if err := envK8s.Create(ctx, zoneWithRestrictions(ns, "dup-cap", &fleetv1.ZoneRestrictions{
		Capabilities: []fleetv1.ZoneCapabilityRestriction{
			{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied},
			{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied},
		},
	})); err == nil {
		t.Error("the API server accepted two capability rules named 'clean.mop'. " +
			"capabilities is +listType=map +listMapKey=name")
	}
}

// ---------------------------------------------------------------------------
// The amendment's own worked YAML, applied verbatim. If the schema cannot take
// the example the specification prints, the specification is wrong or the schema
// is — and an operator copying the example is the first to find out.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneAmendmentWorkedExampleApplies(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// rfcs/amendments/RFC-0001-zone-restrictions.md, A.1 "Schema — Zone Restrictions",
	// spec block only (status there is the Zone Controller's projection, which this
	// item does not own).
	const worked = `
apiVersion: swarmada.io/v1
kind: FleetZone
metadata:
  name: ward-3-bay-2
spec:
  displayName: "Ward 3 — Bay 2"
  restrictions:
    entry:
      mode: Denied
      reason: "outdoors behind the fence; no robot access"
      occupantAction: Report
    capabilities:
      - name: clean.mop
        mode: Denied
        reason: "carpet that must not get wet"
    operating:
      - subject: camera
        mode: Denied
        reason: "patient privacy"
      - subject: microphone
        mode: Denied
        reason: "patient privacy"
      - subject: speed
        mode: Limited
        max: "0.4"
        reason: "occupied ward at night"
      - subject: chemical
        mode: Limited
        allowed: ["quat-ammonium-7"]
        reason: "food preparation surfaces"
      - subject: example.com/leaf-blower
        mode: Denied
`
	var fz fleetv1.FleetZone
	// Strict: an unknown or misspelled field in the amendment's example must fail
	// here rather than be silently dropped by the API server's pruning.
	if err := yaml.UnmarshalStrict([]byte(worked), &fz); err != nil {
		t.Fatalf("the amendment's worked YAML does not decode into the landed types — "+
			"the schema and the specification's own example disagree: %v", err)
	}
	fz.Namespace = ns
	// parentZone: ward-3 is in the amendment's example and omitted here on purpose;
	// the FleetZone webhook rejects a non-existent parent, which is a different rule.
	if err := envK8s.Create(ctx, &fz); err != nil {
		t.Fatalf("the API server rejected the amendment's own worked example: %v", err)
	}

	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "ward-3-bay-2"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	r := got.Spec.Restrictions
	if r == nil || r.Entry == nil || len(r.Capabilities) != 1 || len(r.Operating) != 5 {
		t.Fatalf("the worked example did not round-trip: %#v", r)
	}
	bySubject := map[string]fleetv1.ZoneOperatingRestriction{}
	for _, o := range r.Operating {
		bySubject[o.Subject] = o
	}
	if got := bySubject["speed"].Max; got != "0.4" {
		t.Errorf("speed max came back %q, want \"0.4\" verbatim — an operator's site "+
			"policy must read back as they wrote it, not canonicalised", got)
	}
	if got := bySubject["chemical"].Allowed; len(got) != 1 || got[0] != "quat-ammonium-7" {
		t.Errorf("chemical allowed came back %v, want [quat-ammonium-7]. This field is an "+
			"ALLOW-LIST: everything not listed is denied", got)
	}
	if _, ok := bySubject["example.com/leaf-blower"]; !ok {
		t.Error("the namespaced extension subject was dropped")
	}
}
