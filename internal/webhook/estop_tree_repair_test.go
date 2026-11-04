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

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A zone tree past the bound (two re-parents admitted together can leave one) can be repaired:
// an edit that returns the subtree inside the bound is admitted when no zone stop is issued or in
// force in the namespace, and refused while one stands.
func TestZoneEdit_ATreePastTheBoundIsRepairableWithNoStopStanding(t *testing.T) {
	ctx := context.Background()
	chain := func(stopped bool) []client.Object {
		objs := []client.Object{xoAdapter()}
		for i := 0; i <= 65; i++ {
			parent := ""
			if i > 0 {
				parent = fmt.Sprintf("d%d", i-1)
			}
			z := xoZone(fmt.Sprintf("d%d", i), parent)
			if i == 0 && stopped {
				z.Annotations = map[string]string{estopTriggeredAnnotation: "d0-stop", estopProcessedAnnotation: "d0-stop"}
			}
			objs = append(objs, z)
		}
		return objs
	}
	repair := func(v *FleetZoneValidator) error {
		was := xoZone("d30", "d29")
		now := xoZone("d30", "")
		_, err := v.ValidateUpdate(ctx, was, now)
		return err
	}

	api := xoClient(t, chain(false)...)
	if err := repair(&FleetZoneValidator{Client: api, APIReader: api}); err != nil {
		t.Errorf("with no stop standing, the edit that returns the tree inside the bound was refused: %v", err)
	}

	stopped := xoClient(t, chain(true)...)
	if err := repair(&FleetZoneValidator{Client: stopped, APIReader: stopped}); err == nil {
		t.Errorf("with a stop standing over a tree past the bound, an edit whose loss cannot be judged was admitted")
	}
}
