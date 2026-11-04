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

// ─────────────────────────────────────────────────────────────────────────────
// NO ESTOP NAMES A COMPOSITE AND SILENTLY COVERS NOTHING (RFC-0001 FZ-C4, FZ-C5).
//
// An estop does not follow composition edges, so an estop on a composite stops only the
// robots that declare the composite itself, which the Robot gate keeps at none. Every
// write that could leave an estop trigger on a composite is therefore refused, over the
// wire, with the shipped webhooks:
//
//   - a trigger added to, or re-valued on, a composite;
//   - a composite created already triggered;
//   - one write that adds members and the trigger together;
//   - members added to a zone that already carries a trigger.
//
// A ZoneMaintenance on a composite is the other scope that can name one. It is expanded
// through spec.memberZones, and held with ScopeResolved=False if any member does not
// resolve, so it covers the members or pauses nobody and says why; those paths are the
// ZoneMaintenance controller's tests.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestEnvtest_NoEstopNamesACompositeAndSilentlyCoversNothing(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	for _, m := range []string{"room-a", "room-b"} {
		if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: m, Namespace: w.ns}}); err != nil {
			t.Fatalf("creating member %s: %v", m, err)
		}
	}
	patchZone := func(name string, mutate func(*fleetv1.FleetZone)) error {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		mutate(cur)
		return w.admin.Patch(ctx, cur, client.MergeFrom(base))
	}
	forbidden := func(t *testing.T, what string, err error, wantInErr string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s was ADMITTED: an estop would name a composite and stop none of its members", what)
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), wantInErr) {
			t.Fatalf("%s: refused, but not as expected (want Forbidden containing %q): %v", what, wantInErr, err)
		}
	}

	t.Run("a trigger on a composite", func(t *testing.T) {
		createComposite(t, w, "ward-1", "room-a", "room-b")
		err := patchZone("ward-1", func(z *fleetv1.FleetZone) { z.Annotations[estopTriggeredAnnotation] = "true" })
		forbidden(t, "a trigger on composite ward-1", err, "is a composite zone")
	})

	t.Run("a composite created already triggered", func(t *testing.T) {
		err := w.admin.Create(ctx, &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: "ward-born-stopped", Namespace: w.ns,
				Annotations: map[string]string{estopTriggeredAnnotation: "true"}},
			Spec: fleetv1.FleetZoneSpec{MemberZones: []string{"room-a"}},
		})
		forbidden(t, "a composite created with the trigger", err, "is a composite zone")
	})

	t.Run("members and the trigger in one write", func(t *testing.T) {
		if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "ward-2", Namespace: w.ns}}); err != nil {
			t.Fatal(err)
		}
		err := patchZone("ward-2", func(z *fleetv1.FleetZone) {
			z.Spec.MemberZones = []string{"room-a"}
			z.Annotations[estopTriggeredAnnotation] = "true"
		})
		forbidden(t, "members and a trigger added together", err, "is a composite zone")
	})

	t.Run("members added to a zone under an active estop", func(t *testing.T) {
		if err := w.admin.Create(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "ward-3", Namespace: w.ns}}); err != nil {
			t.Fatal(err)
		}
		if err := patchZone("ward-3", func(z *fleetv1.FleetZone) { z.Annotations[estopTriggeredAnnotation] = "true" }); err != nil {
			t.Fatalf("the estop on terminal zone ward-3 must be admitted: %v", err)
		}
		err := patchZone("ward-3", func(z *fleetv1.FleetZone) { z.Spec.MemberZones = []string{"room-a", "room-b"} })
		forbidden(t, "members added to ward-3 while it is stopped", err, "under an active estop")

		// The estop is untouched by the refusal, and the clear is never refused.
		if err := patchZone("ward-3", func(z *fleetv1.FleetZone) { delete(z.Annotations, estopTriggeredAnnotation) }); err != nil {
			t.Fatalf("clearing the estop on ward-3: %v", err)
		}
		if err := patchZone("ward-3", func(z *fleetv1.FleetZone) { z.Spec.MemberZones = []string{"room-a", "room-b"} }); err != nil {
			t.Fatalf("once the estop is cleared, ward-3 may become a composite: %v", err)
		}
	})
}
