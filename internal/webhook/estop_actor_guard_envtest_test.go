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

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Over the wire, with the shipped webhook manifest and real RBAC: the stamper and the
// validating webhook see the same request, so the stamp the stamper writes is admitted and
// nothing else changes swarmada.io/estop-actor on a stop carrier. The identities hold what
// their names say and no system:masters: a person who may trigger and clear, a ServiceAccount
// that may only trigger, and a person who may only clear. The controller is the manager's
// own ServiceAccount.

// actorWireRoles binds each identity's grant on the three stop carriers in ns.
func actorWireRoles(t *testing.T, w *safetyWire, stopOnlySA string) {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct {
		name    string
		subject rbacv1.Subject
		verbs   []string
	}{
		{"actor-person", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: actorPerson},
			[]string{"get", "list", "watch", "patch", "update", estopTriggerVerb, estopClearVerb}},
		{"actor-stop-only", rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: w.ns, Name: stopOnlySA},
			[]string{"get", "patch", estopTriggerVerb}},
		{"actor-clearer", rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: actorClearer},
			[]string{"get", "patch", estopClearVerb}},
	} {
		if err := rc.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: g.name, Namespace: w.ns},
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{fleetv1.GroupVersion.Group},
				Resources: []string{"fleetzones", "swarmadaconfigs", "robots"}, Verbs: g.verbs}},
		}); err != nil {
			t.Fatalf("creating role %s: %v", g.name, err)
		}
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: g.name, Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: g.name},
			Subjects:   []rbacv1.Subject{g.subject},
		}); err != nil {
			t.Fatalf("binding role %s: %v", g.name, err)
		}
	}
}

// actorWire is one carrier on the wire and the clients that write it.
type actorWire struct {
	t      *testing.T
	w      *safetyWire
	key    client.ObjectKey
	newObj func() client.Object
}

// write merge-patches the carrier's annotations as c, from the object as stored.
func (a actorWire) write(c client.Client, mutate func(ann map[string]string)) error {
	ctx := context.Background()
	obj := a.newObj()
	if err := a.w.admin.Get(ctx, a.key, obj); err != nil {
		a.t.Fatalf("reading %s: %v", a.key, err)
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	mutate(ann)
	obj.SetAnnotations(ann)
	return c.Patch(ctx, obj, client.MergeFrom(base))
}

func (a actorWire) actor() (string, bool) {
	obj := a.newObj()
	if err := a.w.admin.Get(context.Background(), a.key, obj); err != nil {
		a.t.Fatalf("reading %s: %v", a.key, err)
	}
	v, ok := obj.GetAnnotations()[AnnEstopActor]
	return v, ok
}

// refusedByTheActorRule fails unless err is the validating webhook's refusal naming the actor key.
func refusedByTheActorRule(t *testing.T, what string, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("ADMITTED: %s", what)
	case !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "admission webhook") ||
		!strings.Contains(err.Error(), AnnEstopActor):
		t.Errorf("%s was refused, but not by the estop-actor rule: %v", what, err)
	}
}

func (a actorWire) wantActor(what, want string) {
	a.t.Helper()
	got, ok := a.actor()
	switch {
	case want == "" && ok:
		a.t.Errorf("after %s the actor is %q, want none", what, got)
	case want != "" && got != want:
		a.t.Errorf("after %s the actor is %q (present %v), want %q", what, got, ok, want)
	}
}

func setAnn(kv ...string) func(map[string]string) {
	return func(ann map[string]string) {
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(ann, kv[i])
			} else {
				ann[kv[i]] = kv[i+1]
			}
		}
	}
}

func TestEnvtest_EstopActorOnlyCoresStampOverTheWire(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	const saName = "stop-only"
	actorWireRoles(t, w, saName)
	saUser := serviceAccountUserPrefix + w.ns + ":" + saName
	person := asIdentity(t, w, actorPerson)
	stopOnly := asIdentity(t, w, saUser, "system:serviceaccounts", "system:serviceaccounts:"+w.ns)
	clearer := asIdentity(t, w, actorClearer)
	controller := w.as(t, controllerUser)

	if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "dock", Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the zone: %v", err)
	}
	if err := w.admin.Create(ctx, &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the SwarmadaConfig: %v", err)
	}
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(ctx, markerRobot(w.ns, "r-1", nil)); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}

	for _, c := range []struct {
		kind   string
		name   string
		newObj func() client.Object
	}{
		{"FleetZone", "dock", func() client.Object { return &fleetv1.FleetZone{} }},
		{"SwarmadaConfig", "swarmada-config", func() client.Object { return &fleetv1.SwarmadaConfig{} }},
		{"Robot", "r-1", func() client.Object { return &fleetv1.Robot{} }},
	} {
		t.Run(c.kind, func(t *testing.T) {
			a := actorWire{t: t, w: w, key: client.ObjectKey{Namespace: w.ns, Name: c.name}, newObj: c.newObj}

			// The trigger is admitted and stamped with the requester, whatever the client sent.
			if err := a.write(person, setAnn(estopTriggeredAnnotation, "drill", AnnEstopActor, "someone-else")); err != nil {
				t.Fatalf("the person's trigger was refused: %v", err)
			}
			a.wantActor("the person's trigger", actorPerson)

			// While the stop stands and its stamp is unconsumed, nobody changes the actor.
			for _, r := range []struct {
				who    string
				c      client.Client
				mutate func(map[string]string)
			}{
				{"the person rewrites it", person, setAnn(AnnEstopActor, "someone-else")},
				{"the stop-only ServiceAccount rewrites it", stopOnly, setAnn(AnnEstopActor, "someone-else")},
				{"the stop-clear identity rewrites it", clearer, setAnn(AnnEstopActor, actorClearer)},
				{"the controller rewrites it", controller, setAnn(AnnEstopActor, "someone-else")},
				{"the person removes it", person, setAnn(AnnEstopActor, "")},
			} {
				refusedByTheActorRule(t, r.who+" on a standing stop", a.write(r.c, r.mutate))
				a.wantActor(r.who, actorPerson)
			}

			// The controller seals the edge and consumes the stamp in one write.
			if err := a.write(controller, setAnn(estopProcessedAnnotation, "drill", AnnEstopActor, "")); err != nil {
				t.Fatalf("the controller's seal was refused: %v", err)
			}
			a.wantActor("the controller's seal", "")

			// Between edges, nobody writes one, the controller included.
			for _, r := range []struct {
				who string
				c   client.Client
			}{
				{"the person", person}, {"the stop-only ServiceAccount", stopOnly},
				{"the stop-clear identity", clearer}, {"the controller", controller},
			} {
				refusedByTheActorRule(t, r.who+" writes an actor after the seal", a.write(r.c, setAnn(AnnEstopActor, actorPerson)))
				a.wantActor(r.who+"'s write after the seal", "")
			}

			// A write that leaves the actor alone is unaffected.
			if err := a.write(person, setAnn("example.com/note", "night shift")); err != nil {
				t.Errorf("an unrelated annotation write on the stopped %s was refused: %v", c.kind, err)
			}

			// The clear is admitted and stamped with the requester, and the controller consumes it.
			if err := a.write(clearer, setAnn(estopTriggeredAnnotation, "")); err != nil {
				t.Fatalf("the clear was refused: %v", err)
			}
			a.wantActor("the clear", actorClearer)
			if err := a.write(controller, setAnn(estopProcessedAnnotation, "", AnnEstopActor, "")); err != nil {
				t.Fatalf("the controller's seal of the clear was refused: %v", err)
			}
			a.wantActor("the controller's seal of the clear", "")
		})
	}
}

// With the zone's stamper unreachable (failurePolicy Ignore), a stop still lands whenever the
// client leaves the actor alone, and the value a client writes is admitted only if it is the
// requester's own authenticated name.
func TestEnvtest_EstopActorWithTheStamperSkipped(t *testing.T) {
	w := startSafetyWireWithoutZoneStamper(t)
	ctx := context.Background()
	const saName = "stop-only"
	actorWireRoles(t, w, saName)
	saUser := serviceAccountUserPrefix + w.ns + ":" + saName
	person := asIdentity(t, w, actorPerson)
	stopOnly := asIdentity(t, w, saUser, "system:serviceaccounts", "system:serviceaccounts:"+w.ns)
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "dock", Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the zone: %v", err)
	}
	a := actorWire{t: t, w: w, key: client.ObjectKey{Namespace: w.ns, Name: "dock"},
		newObj: func() client.Object { return &fleetv1.FleetZone{} }}

	refusedByTheActorRule(t, "a trigger carrying another name, unstamped",
		a.write(person, setAnn(estopTriggeredAnnotation, "drill", AnnEstopActor, "someone-else")))
	a.wantActor("the refused trigger", "")

	if err := a.write(person, setAnn(estopTriggeredAnnotation, "drill")); err != nil {
		t.Fatalf("a trigger with the stamp skipped was refused: %v", err)
	}
	a.wantActor("the unstamped trigger", "")

	refusedByTheActorRule(t, "the person writes an actor on the standing stop",
		a.write(person, setAnn(AnnEstopActor, actorPerson)))
	refusedByTheActorRule(t, "the stop-only ServiceAccount writes an actor on the standing stop",
		a.write(stopOnly, setAnn(AnnEstopActor, actorPerson)))
	a.wantActor("the refused writes", "")

	if err := a.write(stopOnly, setAnn(estopTriggeredAnnotation, "drill-2", AnnEstopActor, saUser)); err != nil {
		t.Fatalf("a re-trigger carrying the requester's own name was refused: %v", err)
	}
	a.wantActor("the re-trigger", saUser)
}
