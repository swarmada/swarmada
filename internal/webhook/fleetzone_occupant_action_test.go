package webhook

import (
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ADR-0058 §10, as corrected TWICE: at v0.4 all three occupantAction values do the
// SAME thing to robots already in the zone — they NAME them, once, and move none. A manifest setting Immediate must not get silence — an
// operator who believes robots will be pulled out of a zone they just closed has a
// safety expectation the field does not meet.
//
// THE WARNING ITSELF USED TO OVER-CLAIM, and this test used to assert the over-claim.
// It required the warning to say the zone "behaves as occupantAction: Report", which was
// read as: no robot moves, but you WILL be told which robots were inside, by a
// ZoneEntryDeniedWithOccupants event. That event was emitted nowhere. So the assertion
// below no longer looks for "Report" — saying a zone behaves as a value that also does
// nothing conveys nothing — and looks for the corrected substance instead.
//
// REPORT STAYS SILENT, and the reason changed. It used to be silent because it was the
// implemented value; it is silent now because it is the only one of the three whose
// documentation is TRUE, so there is nothing to correct. It is also the DEFAULT: warning
// on it would warn on every entry denial ever written, which is the noise that trains an
// operator to skip warnings.
func TestOccupantAction_UnimplementedModesAreDisclosedNotSilent(t *testing.T) {
	for _, mode := range []fleetv1.ZoneOccupantAction{
		fleetv1.ZoneOccupantGraceful, fleetv1.ZoneOccupantImmediate,
	} {
		z := fzrZone("patio", "", &fleetv1.ZoneRestrictions{
			Entry: &fleetv1.ZoneEntryRestriction{
				Mode: fleetv1.ZoneRestrictionDenied, OccupantAction: mode,
			},
		})
		warnings := occupantActionWarnings(z)
		if len(warnings) == 0 {
			t.Fatalf("occupantAction: %s was accepted in SILENCE. A field that documents a "+
				"behaviour and implements none must say so at the moment it is set", mode)
		}
		w := strings.Join(warnings, " ")
		for _, want := range []string{
			string(mode), "NOT IMPLEMENTED", "inert",
			// The corrected substance, THIRD revision — and the history is the point.
			// The warning first promised an event that was emitted nowhere; that clause
			// was struck and replaced by "no event names them"; the producer then landed
			// and made THAT false. What it must say now is that the MOTION is inert and
			// the REPORT is not.
			"its MOTION is inert", "behaves as occupantAction: Report",
			"ZoneEntryDeniedWithOccupants", "were inside",
			"a report, not a clearance",
			// And the one half that IS real is credited to the entry denial rather than to
			// this field, because an early wording implied this field did it.
			"New work IS withheld",
		} {
			if !strings.Contains(w, want) {
				t.Errorf("warning %q does not carry %q", w, want)
			}
		}
	}

	// Report must be silent — it is the default, and its documentation is now true.
	z := fzrZone("patio", "", &fleetv1.ZoneRestrictions{
		Entry: &fleetv1.ZoneEntryRestriction{
			Mode: fleetv1.ZoneRestrictionDenied, OccupantAction: fleetv1.ZoneOccupantReport,
		},
	})
	if w := occupantActionWarnings(z); len(w) != 0 {
		t.Errorf("Report produced a warning: %v — it is the DEFAULT, so warning on it warns on "+
			"every entry denial ever written, and its documentation no longer promises anything "+
			"it does not do", w)
	}
	// So must a zone with no entry rule at all.
	if w := occupantActionWarnings(fzrZone("plain", "", nil)); len(w) != 0 {
		t.Errorf("a zone with no entry restriction produced a warning: %v", w)
	}
}
