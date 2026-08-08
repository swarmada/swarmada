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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func actionInZone(zone string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: "warehouse-a"},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
	}
}

// spec.zone is immutable: changing it on update is rejected as an Invalid field
// error (ADR-0022 — a re-target would leak the original zone's TDE reservation
// and bypass the per-zone Pending cap).
func TestFleetAction_ZoneImmutableOnUpdate(t *testing.T) {
	v := &FleetActionValidator{}
	cases := []struct {
		name         string
		oldZone      string
		newZone      string
		wantRejected bool
	}{
		{"zone unchanged is allowed", "dock-1", "dock-1", false},
		{"zone changed is rejected", "dock-1", "dock-2", true},
		{"setting a zone from empty is rejected", "", "dock-1", true},
		{"clearing a zone is rejected", "dock-1", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateUpdate(context.Background(), actionInZone(tc.oldZone), actionInZone(tc.newZone))
			if tc.wantRejected {
				if err == nil {
					t.Fatal("expected zone change to be rejected")
				}
				if !apierrors.IsInvalid(err) {
					t.Fatalf("want Invalid, got %T: %v", err, err)
				}
				if !strings.Contains(err.Error(), "spec.zone is immutable") {
					t.Fatalf("rejection must name spec.zone immutability, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("zone-unchanged update must be allowed, got: %v", err)
			}
		})
	}
}
