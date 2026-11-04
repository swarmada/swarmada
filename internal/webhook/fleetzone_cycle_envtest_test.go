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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// cycleRaceRounds is how many times the race below is run. The window is the
// informer lag between two admitted writes, which envtest does not make
// deterministic; repeating it is what gives a cached read the chance to show.
const cycleRaceRounds = 25

// TestEnvtest_ChildThatClosesACycleIsRefusedImmediately closes a parentZone cycle
// with writes that follow each other as fast as a client can send them, and requires
// the closing write to be refused as a cycle every time, over the wire.
//
//   - "new child": create a, create b under a, point a at b.
//   - "re-parented": create a and b parentless and let the cache see both; point b at
//     a, then at once a at b. This is the permissive direction: a cache that still has
//     b parentless sees no cycle.
//
// The unit tests beside the validator pin the reader deterministically; these pin it
// through the served webhook.
func TestEnvtest_ChildThatClosesACycleIsRefusedImmediately(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	create := func(t *testing.T, name, parent string) {
		t.Helper()
		if err := w.admin.Create(ctx, &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
			Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
		}); err != nil {
			t.Fatalf("creating %s (parent %q): %v", name, parent, err)
		}
	}
	setParent := func(name, parent string) error {
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Spec.ParentZone = parent
		return w.admin.Patch(ctx, cur, client.MergeFrom(base))
	}

	for _, variant := range []struct {
		name  string
		setUp func(t *testing.T, a, b string)
	}{
		{"new child", func(t *testing.T, a, b string) {
			create(t, a, "")
			create(t, b, a)
		}},
		{"re-parented", func(t *testing.T, a, b string) {
			create(t, a, "")
			create(t, b, "")
			time.Sleep(200 * time.Millisecond) // let an informer see b parentless
			if err := setParent(b, a); err != nil {
				t.Fatalf("pointing %s at %s: %v", b, a, err)
			}
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			admitted, otherRefusal := 0, 0
			for i := 0; i < cycleRaceRounds; i++ {
				a := fmt.Sprintf("cyc-%s-a-%d", strings.ReplaceAll(variant.name, " ", "-"), i)
				b := fmt.Sprintf("cyc-%s-b-%d", strings.ReplaceAll(variant.name, " ", "-"), i)
				variant.setUp(t, a, b)
				err := setParent(a, b)
				switch {
				case err == nil:
					admitted++
					t.Errorf("round %d: %s.parentZone=%s was ADMITTED; %s → %s → %s is a cycle", i, a, b, a, b, a)
				case !strings.Contains(err.Error(), "cycle"):
					otherRefusal++
					t.Errorf("round %d: refused, but not as a cycle: %v", i, err)
				}
			}
			t.Logf("%s: %d of %d cycle-closing writes admitted, %d refused for another reason",
				variant.name, admitted, cycleRaceRounds, otherRefusal)
		})
	}
}
