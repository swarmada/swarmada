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
	"strings"
	"testing"
	"time"
)

// Rows of the estop hold invariant for an edit that lands after a zone's stop is issued and
// before its fan-out has run (RFC-0001 #safety-estop-scopes). The fan-out resolves its scope
// when it runs, so an edit in that window that takes a zone or a robot out from under the
// stopped zone would leave robots the stop was issued over outside it for good. Each row
// issues the stop with no control plane running, so the window stays open for as long as
// the row needs it.

// tryReparent and tryMove make the edit as the admin and return the API server's answer.
func (r *rig) tryReparent(zone, parent string) error {
	z := r.zoneObj(zone)
	return r.update(r.w.admin, z, false, func() { z.Spec.ParentZone = parent })
}

func (r *rig) tryMove(robot, zone string) error {
	rb := r.robotObj(robot)
	return r.update(r.w.admin, rb, false, func() { rb.Spec.Zone = zone })
}

// refusedUntilApplied requires the edit to be refused while the stop is pending, naming the
// stop and saying that retrying once it is applied works.
func refusedUntilApplied(t *testing.T, what string, err error) {
	t.Helper()
	t.Logf("RESULT %s while anc's stop is pending: %v", what, err)
	if err == nil {
		t.Errorf("INVARIANT: %s was admitted while anc's stop was issued and not yet applied", what)
		return
	}
	if msg := err.Error(); !strings.Contains(msg, `"anc"`) || !strings.Contains(msg, "Retry once the stop is applied") {
		t.Errorf("the refusal of %s does not name the stop and the retry: %v", what, err)
	}
}

func pendingScopeRows() []invariantRow {
	var rows []invariantRow

	// ── Stop, then an edit that would take robots out from under it ──
	for _, edit := range []string{"reparent", "move"} {
		edit := edit
		rows = append(rows, zoneTreeRowsOpts("stop-then-"+edit+"-before-the-fan-out", rigOpts{deferStart: true}, func(t *testing.T, r *rig) {
			r.zone("anc", "", nil)
			r.zone("c", "anc", nil)
			r.zone("elsewhere", "", nil)
			r.robot("rk", "c")
			r.hold("a-held", "rk", true)
			r.trigger(r.zoneObj("anc"), "anc-stop") // issued; no control plane runs to apply it yet
			do := func() error {
				if edit == "reparent" {
					return r.tryReparent("c", "")
				}
				return r.tryMove("rk", "elsewhere")
			}
			refusedUntilApplied(t, edit, do())
			r.start()
			r.waitMarker(r.zoneObj("anc"), true)
			err := do()
			t.Logf("RESULT %s once anc's stop is applied: err=%v", edit, err)
			// Once applied, the re-parent narrows a stop in force, which only its clear releases; the
			// robot's own move is admitted (the stop holds it wherever it goes).
			if edit == "reparent" {
				refusedWhileInForce(t, "the re-parent once anc's stop is applied", `"anc"`, err)
			} else if err != nil {
				t.Errorf("INVARIANT: %s was still refused after anc's stop was applied: %v", edit, err)
			}
			r.assertHeld("rk", true, "a-held")
			if edit == "reparent" {
				r.clearStop(r.zoneObj("anc"))
				r.waitMarker(r.zoneObj("anc"), false)
				if err := do(); err != nil {
					t.Errorf("INVARIANT: the re-parent was still refused after anc's clear was applied: %v", err)
				}
			}
		})...)
	}

	// ── An edit, then the stop: the stop covers the tree as it then stands ──
	rows = append(rows, zoneTreeRowsOpts("reparent-then-stop", rigOpts{}, func(t *testing.T, r *rig) {
		r.zone("anc", "", nil)
		r.zone("c", "anc", nil)
		r.zone("d", "anc", nil) // a robot sits in a leaf zone
		r.robot("ra", "d")
		r.robot("rk", "c")
		r.must("re-parenting c out of anc before any stop", r.tryReparent("c", ""))
		r.trigger(r.zoneObj("anc"), "anc-stop")
		r.waitMarker(r.zoneObj("anc"), true)
		r.assertHeld("ra", true)
		time.Sleep(time.Second)
		if s := r.get("rk").Status.EstopState; stopState(s) {
			t.Errorf("rk, under c, which was re-parented out of anc before anc's stop, is %q", s)
		}
	})...)

	// ── Controls ──
	rows = append(rows, invariantRow{name: "control-move-into-a-zone-under-a-pending-stop", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.zone("anc", "", nil)
			r.zone("c", "anc", nil)
			r.zone("elsewhere", "", nil)
			r.robot("rk", "elsewhere")
			r.trigger(r.zoneObj("anc"), "anc-stop")
			if err := r.tryMove("rk", "c"); err != nil {
				t.Errorf("control: a move INTO a zone under a pending stop was refused: %v", err)
			}
			r.start()
			r.waitMarker(r.zoneObj("anc"), true)
			r.assertHeld("rk", true)
		}})
	rows = append(rows, invariantRow{name: "pending-stop-never-applied-the-edit-stays-refused", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.zone("anc", "", nil)
			r.zone("c", "anc", nil)
			r.zone("elsewhere", "", nil)
			r.robot("rk", "c")
			r.trigger(r.zoneObj("anc"), "anc-stop") // no control plane ever runs
			refusedUntilApplied(t, "reparent", r.tryReparent("c", ""))
			time.Sleep(3 * time.Second)
			refusedUntilApplied(t, "reparent, 3 s later", r.tryReparent("c", ""))
			refusedUntilApplied(t, "move", r.tryMove("rk", "elsewhere"))
			if s := r.get("rk").Spec.Zone; s != "c" {
				t.Errorf("rk's zone is %q", s)
			}
		}})
	return rows
}

// zoneTreeRowsOpts is zoneTreeRows with the rig options the row also needs.
func zoneTreeRowsOpts(name string, opts rigOpts, run func(t *testing.T, r *rig)) []invariantRow {
	zc := opts
	zc.zoneController = true
	return []invariantRow{
		{name: name + "/without-zone-controller", opts: opts, run: run},
		{name: name + "/with-zone-controller", opts: zc, run: run},
	}
}
