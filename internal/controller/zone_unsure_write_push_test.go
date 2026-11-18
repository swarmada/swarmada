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

package controller

import (
	"context"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// A robot enters a zone with an operating rule, and the status write that records it fails with
// an error that is not a conflict, so it may have landed. Its push (FA-R7) is not sent. If the
// write landed, the next frame finds the robot recorded in the zone and moves nothing: it must
// still push the zone's rules, or the adapter keeps running the robot under the rules of where it
// was. If the write did not land, the next frame writes it again and pushes once.
func TestContainmentPush_AWriteOfUnknownOutcomeIsPushedOnceWhatLandedIsRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{name: "the write landed", landed: true},
		{name: "the write did not land"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := lockTestScheme(t)
			ward := restrictedZone("ward", wardPts, denies("camera", "patient privacy"))
			var armed atomic.Bool
			api := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(ward, robotAt("amr-1", "rid1", "")).
				WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if armed.CompareAndSwap(true, false) {
							if tc.landed {
								if err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
									return err
								}
							}
							return apierrors.NewTimeoutError("the server did not answer in time", 0)
						}
						return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			push := &fakeConstraintPush{}
			z := &ZoneController{Client: api, Scheme: scheme, Recorder: record.NewFakeRecorder(64), Constraints: push}
			z.setZones(actionNS, []zone.Candidate{{
				Name: "ward", Floor: 0, Polygon: toZonePoints(wardPts), IsLeaf: true, Depth: 1,
			}})
			inside := func() {
				z.ObservePosition(context.Background(), telemetry.Frame{RobotID: "rid1",
					Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}})
			}

			armed.Store(true)
			inside() // enters the ward; the write fails with a timeout
			if armed.Load() {
				t.Fatal("the frame wrote nothing; the test did not reach the write")
			}
			if n := push.count(); n != 0 {
				t.Fatalf("%d pushes after a failed write, want none", n)
			}
			inside()
			if n := push.count(); n != 1 {
				t.Fatalf("%d pushes once what landed was read, want 1: the adapter runs the robot "+
					"under the rules of the zone it left", n)
			}
			if got := push.last().ContainingZones; len(got) != 1 || got[0] != "ward" {
				t.Fatalf("pushed containment %v, want [ward]", got)
			}
			inside()
			if n := push.count(); n != 1 {
				t.Fatalf("%d pushes after a frame that moves nothing, want 1", n)
			}
			rb := &fleetv1.Robot{}
			if err := api.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "amr-1"}, rb); err != nil {
				t.Fatal(err)
			}
			if len(rb.Status.ContainingZones) != 1 || rb.Status.ContainingZones[0] != "ward" {
				t.Fatalf("recorded containment %v, want [ward]", rb.Status.ContainingZones)
			}
		})
	}
}
