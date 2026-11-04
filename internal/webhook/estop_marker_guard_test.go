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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const controllerUser = "system:serviceaccount:swarmada-system:swarmada-manager"

func markerAnn(v string) map[string]string {
	if v == "" {
		return nil
	}
	return map[string]string{estopProcessedAnnotation: v}
}

// A principal that is not the control-plane controller may not write the
// estop idempotence marker: writing it ahead of a trigger suppresses the fan-out
// silently, with no estop delivered and no audit entry.
func TestEstopMarkerGuard_ClientWriteRefused(t *testing.T) {
	cases := []struct {
		name           string
		user           string
		oldVal, newVal string
	}{
		{"operator adds the marker", "alice", "", "e1"},
		{"operator re-values the marker", "alice", "e1", "e2"},
		{"operator removes the marker", "alice", "e1", ""},
		{"another service account adds it", "system:serviceaccount:tenant-a:wms-bridge", "", "e1"},
		{"cluster admin adds it", "kubernetes-admin", "", "e1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardEstopProcessedMarker(ctxAsUser(tc.user), fleetZoneGR, zoneNS, "floor-1",
				markerAnn(tc.oldVal), markerAnn(tc.newVal))
			if err == nil || !apierrors.IsForbidden(err) {
				t.Fatalf("a client write of the estop marker must be Forbidden, got: %v", err)
			}
		})
	}
}

// The controller's own write is the one legitimate one: it is how the fan-out
// records that this trigger value has been acted on.
func TestEstopMarkerGuard_ControllerWriteAllowed(t *testing.T) {
	if err := GuardEstopProcessedMarker(ctxAsUser(controllerUser), fleetZoneGR, zoneNS, "floor-1",
		nil, markerAnn("e1")); err != nil {
		t.Fatalf("the controller must be able to record the marker: %v", err)
	}
	// And to drop it again on an estop-clear.
	if err := GuardEstopProcessedMarker(ctxAsUser(controllerUser), fleetZoneGR, zoneNS, "floor-1",
		markerAnn("e1"), nil); err != nil {
		t.Fatalf("the controller must be able to drop the marker: %v", err)
	}
}

// The controller is recognised as exactly the principal it runs as. The same
// ServiceAccount name in another namespace is a different principal, one anybody able
// to create a ServiceAccount there could mint a token for, and it is refused.
func TestEstopMarkerGuard_ControllerIdentityMatching(t *testing.T) {
	cases := []struct {
		user string
		want bool
	}{
		{"system:serviceaccount:swarmada-system:swarmada-manager", true},
		{"system:serviceaccount:fleet-ops:swarmada-manager", false},
		{"system:serviceaccount:swarmada-system:swarmada-manager-2", false},
		{"system:serviceaccount::swarmada-manager", false},
		{"system:serviceaccount:swarmada-system", false},
		{"swarmada-manager", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isControllerServiceAccount(tc.user); got != tc.want {
			t.Errorf("isControllerServiceAccount(%q) = %v, want %v", tc.user, got, tc.want)
		}
	}
}

// An update that does not touch the marker is not a marker write. Without this the
// guard would deny every unrelated edit of a zone that is currently estopped.
func TestEstopMarkerGuard_UnchangedMarkerAdmitted(t *testing.T) {
	if err := GuardEstopProcessedMarker(ctxAsUser("alice"), fleetZoneGR, zoneNS, "floor-1",
		markerAnn("e1"), markerAnn("e1")); err != nil {
		t.Fatalf("an update leaving the marker unchanged must be admitted: %v", err)
	}
	if err := GuardEstopProcessedMarker(ctxAsUser("alice"), fleetZoneGR, zoneNS, "floor-1",
		nil, nil); err != nil {
		t.Fatalf("an update with no marker on either side must be admitted: %v", err)
	}
}

// A request the webhook cannot attribute is refused, not admitted: the guard fails
// closed like the estop verb checks it sits beside.
func TestEstopMarkerGuard_UnattributableRequestRefused(t *testing.T) {
	err := GuardEstopProcessedMarker(context.Background(), fleetZoneGR, zoneNS, "floor-1",
		nil, markerAnn("e1"))
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a marker write with no admission request must be Forbidden, got: %v", err)
	}
}
