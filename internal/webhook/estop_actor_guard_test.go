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
	"fmt"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// swarmada.io/estop-actor is CORE's record of who triggered or cleared a stop. The stamper
// writes it from the authenticated requester when the trigger changes, and the controller
// removes it once it has sealed the edge. These tests assert that no other change to it is
// admitted on any stop carrier, whoever sends it, and that the stamper's own writes and the
// controller's removal still are.

const (
	actorPerson  = "kit-operator"
	actorClearer = "shift-lead"
)

// actorStopOnlySA is a ServiceAccount that may trigger a stop and nothing more.
var actorStopOnlySA = serviceAccountUserPrefix + zoneNS + ":stop-only"

// actorCtx is an admission context for username in groups. Unlike ctxAsUser it carries the
// groups, so a ServiceAccount arrives the way the API server sends one.
func actorCtx(username string, groups ...string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{Username: username, Groups: groups},
		},
	})
}

func actorStopOnlyCtx() context.Context {
	return actorCtx(actorStopOnlySA, "system:serviceaccounts", "system:serviceaccounts:"+zoneNS, "system:authenticated")
}

// actorAnns builds annotations from key/value pairs.
func actorAnns(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func copyAnns(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// actorCarrier runs one stop carrier's validating webhook on a write of its annotations.
// label adds a label to the new object, so a write is not annotations alone.
type actorCarrier struct {
	kind   string
	update func(t *testing.T, ctx context.Context, oldAnn, newAnn map[string]string, label bool) error
	create func(t *testing.T, ctx context.Context, ann map[string]string) error
}

func actorCarriers() []actorCarrier {
	allow := func() *fakeEstopAuthz { return &fakeEstopAuthz{allow: true} }
	return []actorCarrier{
		{
			kind: "FleetZone",
			update: func(t *testing.T, ctx context.Context, oldAnn, newAnn map[string]string, label bool) error {
				v := newZoneValidator(t)
				v.EstopAuthz = allow()
				o, n := estopZone("dock", ""), estopZone("dock", "")
				o.Annotations, n.Annotations = copyAnns(oldAnn), copyAnns(newAnn)
				if label {
					n.Labels = map[string]string{"team": "night"}
				}
				_, err := v.ValidateUpdate(ctx, o, n)
				return err
			},
			create: func(t *testing.T, ctx context.Context, ann map[string]string) error {
				v := newZoneValidator(t)
				v.EstopAuthz = allow()
				z := estopZone("dock", "")
				z.Annotations = copyAnns(ann)
				_, err := v.ValidateCreate(ctx, z)
				return err
			},
		},
		{
			kind: "SwarmadaConfig",
			update: func(_ *testing.T, ctx context.Context, oldAnn, newAnn map[string]string, label bool) error {
				v := &SwarmadaConfigValidator{EstopAuthz: allow()}
				o, n := markedConfig(""), markedConfig("")
				o.Annotations, n.Annotations = copyAnns(oldAnn), copyAnns(newAnn)
				if label {
					n.Labels = map[string]string{"team": "night"}
				}
				_, err := v.ValidateUpdate(ctx, o, n)
				return err
			},
			create: func(_ *testing.T, ctx context.Context, ann map[string]string) error {
				v := &SwarmadaConfigValidator{EstopAuthz: allow()}
				c := markedConfig("")
				c.Annotations = copyAnns(ann)
				_, err := v.ValidateCreate(ctx, c)
				return err
			},
		},
		{
			kind: "Robot",
			update: func(t *testing.T, ctx context.Context, oldAnn, newAnn map[string]string, label bool) error {
				g := estopGate(t, allow())
				o, n := robotForEstop("amr-1", ""), robotForEstop("amr-1", "")
				o.Annotations, n.Annotations = copyAnns(oldAnn), copyAnns(newAnn)
				if label {
					n.Labels = map[string]string{"team": "night"}
				}
				_, err := g.ValidateUpdate(ctx, o, n)
				return err
			},
			create: func(t *testing.T, ctx context.Context, ann map[string]string) error {
				// A create runs the adapter gate, so the adapter the robot names exists.
				g := &RobotAdmissionGate{Client: xoClient(t, xoZone(robotEstopZone, ""), xoAdapter()), EstopAuthz: allow()}
				r := xoRobot("amr-1", robotEstopZone, "")
				r.Annotations = copyAnns(ann)
				_, err := g.ValidateCreate(ctx, r)
				return err
			},
		},
	}
}

const (
	actTrig   = estopTriggeredAnnotation
	actMarker = estopProcessedAnnotation
	actKey    = AnnEstopActor
)

type actorRow struct {
	name     string
	ctx      func() context.Context
	old, new map[string]string
	label    bool
	admitted bool
}

// actorUpdateRows are the update rows every stop carrier answers the same way.
func actorUpdateRows() []actorRow {
	person := func() context.Context { return actorCtx(actorPerson) }
	other := func() context.Context { return actorCtx("mallory") }
	clearer := func() context.Context { return actorCtx(actorClearer) }
	controller := func() context.Context {
		return actorCtx(controllerUser, "system:serviceaccounts", "system:serviceaccounts:swarmada-system")
	}
	standing := actorAnns(actTrig, "t1", actKey, actorPerson) // a stop in force, its stamp not yet consumed
	sealed := actorAnns(actTrig, "t1", actMarker, "t1")       // a stop in force, its edge sealed and the stamp consumed
	return []actorRow{
		// Refused: a change to the actor that is not the stamper's at a change of the trigger.
		{name: "person-rewrites-the-actor-of-a-standing-stop", ctx: other,
			old: standing, new: actorAnns(actTrig, "t1", actKey, "someone-else")},
		{name: "person-writes-an-actor-after-the-seal", ctx: other,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},
		{name: "person-writes-its-own-name-after-the-seal", ctx: other,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, "mallory")},
		{name: "person-removes-the-actor-before-the-seal", ctx: other,
			old: standing, new: actorAnns(actTrig, "t1")},
		{name: "stop-only-serviceaccount-rewrites-the-actor", ctx: actorStopOnlyCtx,
			old: standing, new: actorAnns(actTrig, "t1", actKey, "someone-else")},
		{name: "stop-only-serviceaccount-writes-an-actor-after-the-seal", ctx: actorStopOnlyCtx,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},
		{name: "stop-clear-identity-rewrites-the-actor", ctx: clearer,
			old: standing, new: actorAnns(actTrig, "t1", actKey, actorClearer)},
		{name: "stop-clear-identity-writes-an-actor-after-the-seal", ctx: clearer,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},
		{name: "controller-writes-an-actor-after-the-seal", ctx: controller,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},
		{name: "controller-rewrites-the-actor", ctx: controller,
			old: standing, new: actorAnns(actTrig, "t1", actKey, "someone-else")},
		{name: "controller-seals-and-writes-an-actor", ctx: controller,
			old: actorAnns(actTrig, "t1"), new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},
		{name: "trigger-carrying-another-name", ctx: person,
			old: nil, new: actorAnns(actTrig, "t1", actKey, "someone-else")},
		{name: "re-trigger-carrying-another-name", ctx: person,
			old: sealed, new: actorAnns(actTrig, "t2", actMarker, "t1", actKey, "someone-else")},
		{name: "clear-carrying-another-name", ctx: clearer,
			old: sealed, new: actorAnns(actMarker, "t1", actKey, actorPerson)},
		{name: "actor-change-alongside-a-label", ctx: other, label: true,
			old: standing, new: actorAnns(actTrig, "t1", actKey, "someone-else")},
		{name: "unattributable-request", ctx: context.Background,
			old: sealed, new: actorAnns(actTrig, "t1", actMarker, "t1", actKey, actorPerson)},

		// Admitted: the stamper's output at a change of the trigger, the controller's removal,
		// and every write that leaves the actor alone.
		{name: "trigger-stamped-with-the-requester", ctx: person, admitted: true,
			old: nil, new: actorAnns(actTrig, "t1", actKey, actorPerson)},
		{name: "trigger-with-the-stamp-skipped", ctx: person, admitted: true,
			old: nil, new: actorAnns(actTrig, "t1")},
		{name: "stop-only-serviceaccount-trigger-stamped-with-its-name", ctx: actorStopOnlyCtx, admitted: true,
			old: nil, new: actorAnns(actTrig, "t1", actKey, actorStopOnlySA)},
		{name: "re-trigger-stamped-with-the-requester", ctx: person, admitted: true,
			old: sealed, new: actorAnns(actTrig, "t2", actMarker, "t1", actKey, actorPerson)},
		{name: "clear-stamped-with-the-requester", ctx: clearer, admitted: true,
			old: sealed, new: actorAnns(actMarker, "t1", actKey, actorClearer)},
		{name: "clear-with-no-identity-drops-the-stale-stamp", ctx: func() context.Context { return actorCtx("") },
			admitted: true, old: standing, new: nil},
		{name: "controller-seals-the-trigger", ctx: controller, admitted: true,
			old: standing, new: sealed},
		{name: "controller-seals-the-clear", ctx: controller, admitted: true,
			old: actorAnns(actMarker, "t1", actKey, actorClearer), new: nil},
		{name: "unrelated-write-leaves-the-actor-alone", ctx: other, label: true, admitted: true,
			old: standing, new: standing},
	}
}

func TestEstopActor_OnlyCoresStampChangesIt_Update(t *testing.T) {
	for _, c := range actorCarriers() {
		for _, row := range actorUpdateRows() {
			t.Run(c.kind+"/"+row.name, func(t *testing.T) {
				err := c.update(t, row.ctx(), row.old, row.new, row.label)
				switch {
				case row.admitted && err != nil:
					t.Fatalf("refused: %v", err)
				case !row.admitted && err == nil:
					t.Fatalf("ADMITTED: %s on a %s changed %s and is not the control plane's own stamp at a trigger change",
						row.name, c.kind, AnnEstopActor)
				case !row.admitted && !apierrors.IsForbidden(err):
					t.Fatalf("refused, but not as Forbidden: %v", err)
				}
			})
		}
	}
}

// On a Robot the stamper also writes the actor when the operator's release request changes,
// so that change is a stamp edge there too, and only there.
func TestEstopActor_OnlyCoresStampChangesIt_RobotRelease(t *testing.T) {
	person := func() context.Context { return actorCtx(actorPerson) }
	sealed := actorAnns(actMarker, "t1")
	for _, row := range []actorRow{
		{name: "release-stamped-with-the-requester", ctx: person, admitted: true,
			old: sealed, new: actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1", actKey, actorPerson)},
		{name: "release-carrying-another-name", ctx: person,
			old: sealed, new: actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1", actKey, "someone-else")},
		{name: "release-withdrawn-stamped-with-the-requester", ctx: person, admitted: true,
			old: actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1"), new: actorAnns(actMarker, "t1", actKey, actorPerson)},
		{name: "controller-claims-the-release-and-consumes-the-stamp",
			ctx: func() context.Context { return actorCtx(controllerUser) }, admitted: true,
			old: actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1", actKey, actorPerson),
			new: actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1", fleetv1.EstopResumeClaimedAnnotation, "r1")},
	} {
		t.Run(row.name, func(t *testing.T) {
			g := estopGate(t, &fakeEstopAuthz{allow: true})
			o, n := robotForEstop("amr-1", ""), robotForEstop("amr-1", "")
			o.Annotations, n.Annotations = copyAnns(row.old), copyAnns(row.new)
			_, err := g.ValidateUpdate(row.ctx(), o, n)
			if row.admitted && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !row.admitted && !apierrors.IsForbidden(err) {
				t.Fatalf("err = %v, want Forbidden", err)
			}
		})
	}
	// A FleetZone has no release request: the key does not make a stamp edge there.
	t.Run("fleetzone-release-key-is-not-a-stamp-edge", func(t *testing.T) {
		v := newZoneValidator(t)
		v.EstopAuthz = &fakeEstopAuthz{allow: true}
		o, n := estopZone("dock", ""), estopZone("dock", "")
		o.Annotations = actorAnns(actMarker, "t1")
		n.Annotations = actorAnns(actMarker, "t1", fleetv1.EstopResumeAnnotation, "r1", actKey, actorPerson)
		if _, err := v.ValidateUpdate(person(), o, n); !apierrors.IsForbidden(err) {
			t.Fatalf("an actor written beside a release key on a FleetZone: err = %v, want Forbidden", err)
		}
	})
}

func TestEstopActor_OnlyCoresStampChangesIt_Create(t *testing.T) {
	for _, c := range actorCarriers() {
		for _, row := range []struct {
			name     string
			ann      map[string]string
			admitted bool
		}{
			{"born-with-an-actor-and-no-trigger", actorAnns(actKey, actorPerson), false},
			{"born-triggered-carrying-another-name", actorAnns(actTrig, "t1", actKey, "someone-else"), false},
			{"born-triggered-stamped-with-the-requester", actorAnns(actTrig, "t1", actKey, actorPerson), true},
			{"born-triggered-with-the-stamp-skipped", actorAnns(actTrig, "t1"), true},
			{"born-with-neither", nil, true},
		} {
			t.Run(c.kind+"/"+row.name, func(t *testing.T) {
				err := c.create(t, actorCtx(actorPerson), row.ann)
				if row.admitted && err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !row.admitted && !apierrors.IsForbidden(err) {
					t.Fatalf("err = %v, want Forbidden", err)
				}
			})
		}
	}
}

// Whatever the client sends at a change of the trigger, the stamper's output is admitted, so
// the rule never refuses a stop the stamper served. Each case runs the real stamping code on
// the client's object, as the mutating webhook does before validation.
func TestEstopActor_EveryStamperOutputAtAnEdgeIsAdmitted(t *testing.T) {
	type state struct{ trigger, actor string } // "" is absent
	olds := []state{{}, {"t1", ""}, {"t1", actorPerson}, {"t1", "someone-else"}}
	triggers := []string{"", "t1", "t2"}
	sent := []string{"<unchanged>", "", actorPerson, "someone-else"}
	users := []string{actorPerson, actorStopOnlySA, ""}
	for _, c := range actorCarriers() {
		for _, o := range olds {
			for _, nt := range triggers {
				if nt == o.trigger {
					continue // not a stamp edge
				}
				for _, s := range sent {
					for _, user := range users {
						oldAnn := map[string]string{}
						if o.trigger != "" {
							oldAnn[actTrig] = o.trigger
						}
						if o.actor != "" {
							oldAnn[actKey] = o.actor
						}
						newAnn := copyAnns(oldAnn)
						delete(newAnn, actTrig)
						if nt != "" {
							newAnn[actTrig] = nt
						}
						switch s {
						case "<unchanged>":
						case "":
							delete(newAnn, actKey)
						default:
							newAnn[actKey] = s
						}
						obj := estopZone("dock", "")
						obj.Annotations = newAnn
						ctx := ctxWithUser(t, user, oldAnn)
						stampEstopActor(ctx, obj)
						if c.kind == "Robot" {
							stampActorOn(ctx, obj, fleetv1.EstopResumeAnnotation)
						}
						name := fmt.Sprintf("%s/old=%v/trigger=%q/sent=%q/user=%q", c.kind, o, nt, s, user)
						t.Run(name, func(t *testing.T) {
							if err := c.update(t, actorCtx(user), oldAnn, obj.Annotations, false); err != nil {
								t.Fatalf("the stamper's own output was refused (actor %q): %v", obj.Annotations[actKey], err)
							}
						})
					}
				}
			}
		}
	}
}
