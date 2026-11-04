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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FZ-R7 and FZ-R8 — admission refuses a new hole, and never refuses a new wall.
//
// THE ASYMMETRY IS THE SAFETY PROPERTY, and symmetric validation is the bug this
// file exists to prevent. It is the natural thing to write: "reject any write that
// makes an ancestor and a descendant disagree" is one rule instead of two and reads
// as more principled. It is wrong in a way that matters — refusing a parent's
// TIGHTENING on account of a child would make a boundary un-addable, would let a
// leaf someone else owns veto a site policy, and would force the webhook to list a
// whole subtree on every zone write.
//
// The direction of the asymmetry is what makes it safe: the unresolvable case is
// never the permissive one.
//
// The fake client is the right client here. This is a pure ancestor-walk over
// objects the validator READS; there is no CEL, no status subresource and no
// defaulting involved, so envtest would add a control plane and prove nothing more.
// (Contrast internal/controller's projection tests, which turn on generation and the
// status subresource and are therefore envtest.)

func fzrScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func fzrZone(name, parent string, r *fleetv1.ZoneRestrictions) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent, Restrictions: r},
	}
}

func op(subject string, mode fleetv1.ZoneRestrictionMode, max string, allowed []string) fleetv1.ZoneOperatingRestriction {
	return fleetv1.ZoneOperatingRestriction{Subject: subject, Mode: mode, Max: max, Allowed: allowed}
}

// validateFZR runs the FleetZone validator against a tree, returning the error for
// the zone under test.
func validateFZR(t *testing.T, under *fleetv1.FleetZone, tree ...*fleetv1.FleetZone) error {
	t.Helper()
	objs := make([]runtime.Object, 0, len(tree))
	for _, z := range tree {
		objs = append(objs, z)
	}
	c := fake.NewClientBuilder().WithScheme(fzrScheme(t)).WithRuntimeObjects(objs...).Build()
	v := &FleetZoneValidator{Client: c, APIReader: c}
	_, err := v.ValidateCreate(context.Background(), under)
	return err
}

// ---------------------------------------------------------------------------
// FZ-R7 — a descendant may not weaken an ancestor. Rejected, naming the ancestor.
// ---------------------------------------------------------------------------

func TestFZR7_DescendantWeakeningAnAncestorIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ancestor  *fleetv1.ZoneRestrictions
		child     *fleetv1.ZoneRestrictions
		wantInErr []string
	}{
		{
			name:     "raised max",
			ancestor: &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("speed", fleetv1.ZoneRestrictionLimited, "0.4", nil)}},
			child:    &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("speed", fleetv1.ZoneRestrictionLimited, "2.0", nil)}},
			// The ancestor zone and the conflicting rule, both named (FZ-R7).
			wantInErr: []string{"site", "speed", "0.4"},
		},
		{
			name:      "widened allow-list",
			ancestor:  &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"quat-ammonium-7"})}},
			child:     &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"quat-ammonium-7", "bleach"})}},
			wantInErr: []string{"site", "chemical", "bleach"},
		},
		{
			name:      "Limited under an ancestor's Denied",
			ancestor:  &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionDenied, "", nil)}},
			child:     &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionLimited, "3", nil)}},
			wantInErr: []string{"site", "camera"},
		},
		{
			name:      "Required under an ancestor's Denied",
			ancestor:  &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionDenied, "", nil)}},
			child:     &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionRequired, "", nil)}},
			wantInErr: []string{"site", "camera"},
		},
		{
			name:      "Denied under an ancestor's Required (the reverse)",
			ancestor:  &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionRequired, "", nil)}},
			child:     &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionDenied, "", nil)}},
			wantInErr: []string{"site", "camera"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := fzrZone("site", "", tc.ancestor)
			child := fzrZone("bay", "site", tc.child)
			err := validateFZR(t, child, site)
			if err == nil {
				t.Fatalf("admitted a descendant that weakens its ancestor. FZ-R7: this is " +
					"REJECTED at admission, never silently ignored — silently ignoring produces " +
					"an object whose spec says one thing and whose behaviour is another, and the " +
					"purpose of this surface is that the API is where the rule is stated")
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q; FZ-R7 requires the ancestor zone and "+
						"the conflicting rule", err.Error(), want)
				}
			}
		})
	}
}

func TestFZR7_RejectionReachesThroughAGrandparent(t *testing.T) {
	// The weakened rule is two levels up and the middle zone says nothing. A walk
	// that only compares against the direct parent misses this entirely, and it is
	// the shape a real site policy takes: the rule is on the building, the edit is
	// on a room.
	site := fzrZone("site", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionDenied, "", nil)},
	})
	ward := fzrZone("ward-3", "site", nil)
	bay := fzrZone("bay-2", "ward-3", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionLimited, "3", nil)},
	})
	err := validateFZR(t, bay, site, ward)
	if err == nil {
		t.Fatal("a grandparent's denial was not enforced against a grandchild")
	}
	if !strings.Contains(err.Error(), "site") {
		t.Errorf("error %q does not name the DECLARING ancestor (site) — naming the direct "+
			"parent would send the operator to a zone that declares nothing", err.Error())
	}
}

func TestFZR7_EntryAndCapabilityAxesAreCoveredToo(t *testing.T) {
	// The three axes are one model; an admission rule that covers only the axis it
	// was written for is the "three schemas" mistake ADR-0058 §1 refuses.
	site := fzrZone("site", "", &fleetv1.ZoneRestrictions{
		Entry:        &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied},
		Capabilities: []fleetv1.ZoneCapabilityRestriction{{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied}},
	})
	// A descendant cannot re-permit entry: there is no permissive mode to write, so
	// the only way to weaken is to say nothing — which is NOT a weakening, because
	// the ancestor's rule still applies. This asserts the non-obvious half: a child
	// declaring nothing must be ADMITTED.
	quiet := fzrZone("bay", "site", nil)
	if err := validateFZR(t, quiet, site); err != nil {
		t.Errorf("a descendant that declares nothing was rejected: %v. Absence is not a "+
			"weakening — the ancestor's rule still applies to it", err)
	}
	// And re-declaring the same denial is a no-op, not a conflict.
	same := fzrZone("bay", "site", &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied},
	})
	if err := validateFZR(t, same, site); err != nil {
		t.Errorf("a descendant restating its ancestor's denial was rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// FZ-R8 — an ancestor tightening is ALWAYS admitted. The other direction.
// ---------------------------------------------------------------------------

func TestFZR8_AncestorTighteningIsAlwaysAdmittedEvenWhenADescendantContradictsIt(t *testing.T) {
	// The existing descendant is more permissive than the rule being added. A
	// symmetric check would reject this write — and that is the bug.
	child := fzrZone("bay", "site", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("speed", fleetv1.ZoneRestrictionLimited, "2.0", nil)},
	})
	site := fzrZone("site", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("speed", fleetv1.ZoneRestrictionLimited, "0.4", nil)},
	})

	if err := validateFZR(t, site, child); err != nil {
		t.Fatalf("an ANCESTOR tightening onto an existing descendant was rejected: %v\n"+
			"FZ-R8: this write is ALWAYS admitted. Refusing it would make a boundary "+
			"un-addable, would let a leaf someone else owns veto a site policy, and would "+
			"require the webhook to list a whole subtree on every zone write. The "+
			"contradiction resolves per FZ-R1/FZ-R3 and the DESCENDANT publishes "+
			"RestrictionsConsistent: False — it is reported, not refused", err)
	}

	// The sharpest case: an ancestor adds a Denied while a descendant already
	// Requires the same subject. Admitted here; resolved to Denied by the resolver;
	// reported on the descendant.
	requiring := fzrZone("bay", "site", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionRequired, "", nil)},
	})
	denying := fzrZone("site", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{op("camera", fleetv1.ZoneRestrictionDenied, "", nil)},
	})
	if err := validateFZR(t, denying, requiring); err != nil {
		t.Errorf("an ancestor adding a Denied over a descendant's Required was rejected: %v. "+
			"That contradiction is FZ-R3's case and belongs on the descendant's "+
			"RestrictionsConsistent condition, not in a refusal of the parent's write", err)
	}
}

func TestFZR8_ANewRootZoneWithRestrictionsIsAdmitted(t *testing.T) {
	// No ancestors at all: nothing to compare against, so nothing to reject.
	root := fzrZone("site", "", &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied, Reason: "patio"},
	})
	if err := validateFZR(t, root); err != nil {
		t.Errorf("a root zone declaring a restriction was rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The walk must not become a second walk (the prompt's constraint), and it must
// not change what the cycle check already did.
// ---------------------------------------------------------------------------

func TestFZR7_DoesNotDisturbTheExistingCycleAndParentChecks(t *testing.T) {
	// A missing parent is still the parent-existence error, not a restriction error.
	orphan := fzrZone("bay", "nope", nil)
	err := validateFZR(t, orphan)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a missing parentZone produced %v; want the existing parent-existence rejection", err)
	}

	// A cycle is still reported as a cycle.
	a := fzrZone("a", "b", nil)
	b := fzrZone("b", "a", nil)
	err = validateFZR(t, a, a, b)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("a parentZone cycle produced %v; want the existing cycle rejection", err)
	}
}
