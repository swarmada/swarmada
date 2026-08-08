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

package registrar

// Discover must not read a just-created DiscoveredRobot back from a (possibly
// lagging) client cache: Client.Create() already populates the object, so status
// is written on it directly. (fleetAdapter/verifiedTLS/adapterID/regRobotID live in
// the sibling *_test.go files.)

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A first Discover of a new robot_id succeeds and writes status even when the
// client NEVER serves a read of the freshly-created DiscoveredRobot (the informer
// cache lag that previously produced a spurious NotFound). This proves Discover has
// no Get-after-Create dependency: it writes status on the Create-populated object.
func TestDiscover_NoGetAfterCreate_CacheLag(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	var captured fleetv1.DiscoveredRobotStatus
	statusWrites := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fleetAdapter("amr-adapter", "amr-device")).
		WithStatusSubresource(&fleetv1.DiscoveredRobot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// Simulate a lagging cache: a DiscoveredRobot read never succeeds. Other
			// reads (Robot admitted-check, FleetAdapter for the suggested class) pass
			// through so the rest of Discover behaves normally.
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*fleetv1.DiscoveredRobot); ok {
					return apierrors.NewNotFound(schema.GroupResource{Group: "swarmada.io", Resource: "discoveredrobots"}, key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if dr, ok := obj.(*fleetv1.DiscoveredRobot); ok {
					captured = dr.Status
					statusWrites++
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	fixed := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	r := &Registrar{Client: c, Now: func() time.Time { return fixed }}

	ack := r.Discover(context.Background(), adapterID(), verifiedTLS("amr-adapter"),
		&fav1.DiscoverRobot{RobotId: regRobotID})

	if !ack.GetAccepted() {
		t.Fatalf("Discover must succeed despite the cache never serving the freshly-created object; got %+v", ack)
	}
	if statusWrites != 1 {
		t.Errorf("status must be written exactly once, got %d", statusWrites)
	}
	if captured.RobotID != regRobotID {
		t.Errorf("Status.RobotID = %q, want %q (required, minLength 1)", captured.RobotID, regRobotID)
	}
	if captured.Phase != fleetv1.DiscoveredRobotPhaseDiscovered {
		t.Errorf("phase = %q, want Discovered", captured.Phase)
	}
	if captured.TTLExpiresAt == nil {
		t.Error("ttlExpiresAt must be written")
	}
	if captured.SuggestedRobotClass != "amr-device" {
		t.Errorf("SuggestedRobotClass = %q, want amr-device (ADR-0027)", captured.SuggestedRobotClass)
	}
}

// The status write must satisfy the CRD's required status.robotId (minLength 1):
// an empty RobotID would be rejected by the apiserver. This models that validation
// with an interceptor and asserts Discover writes a non-empty RobotID == robot_id.
func TestDiscover_StatusRobotIDPassesValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&fleetv1.DiscoveredRobot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if dr, ok := obj.(*fleetv1.DiscoveredRobot); ok && dr.Status.RobotID == "" {
					return apierrors.NewInvalid(
						schema.GroupKind{Group: "swarmada.io", Kind: "DiscoveredRobot"}, dr.Name,
						field.ErrorList{field.Invalid(field.NewPath("status", "robotId"), "", "should be at least 1 chars long")})
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &Registrar{Client: c, Now: func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }}

	ack := r.Discover(context.Background(), adapterID(), controlstream.TLSIdentity{},
		&fav1.DiscoverRobot{RobotId: regRobotID})
	if !ack.GetAccepted() {
		t.Fatalf("Discover must succeed (non-empty RobotID passes minLength validation); got %+v", ack)
	}
	dr := &fleetv1.DiscoveredRobot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: regNS, Name: regRobotID}, dr); err != nil {
		t.Fatalf("get DiscoveredRobot: %v", err)
	}
	if dr.Status.RobotID != regRobotID {
		t.Errorf("Status.RobotID = %q, want %q", dr.Status.RobotID, regRobotID)
	}
}
