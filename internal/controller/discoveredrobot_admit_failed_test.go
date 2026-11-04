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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
)

// `swarmctl admit` reports success when the admission is recorded, so a create the control plane
// cannot complete is invisible unless it is recorded where the operator looks: an
// AdmissionFailed Event on the DiscoveredRobot (docs/operations.md, "If the control plane cannot
// complete a marked admission").

// newRefusingDRReconciler builds a reconciler whose Robot creates are answered by refuse (nil
// lets the create through) and counted.
func newRefusingDRReconciler(t *testing.T, refuse error, objs ...client.Object) (*DiscoveredRobotReconciler, *record.FakeRecorder, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	creates := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.DiscoveredRobot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*fleetv1.Robot); ok {
					creates++
					if refuse != nil {
						return refuse
					}
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	rec := record.NewFakeRecorder(16)
	now := drBase
	return &DiscoveredRobotReconciler{Client: c, Recorder: rec, now: func() time.Time { return now }}, rec, &creates
}

func drainDREvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func reconcileDRErr(r *DiscoveredRobotReconciler, name string) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: drNS, Name: name}})
	return err
}

// The Robot admission gate refuses while the adapter is not Connected. That refusal can clear
// without the admission changing, so it is retried — and each refusal is recorded, because the
// operator was told the admission succeeded.
func TestOperatorAdmit_RefusedCreateRecordsAdmissionFailedAndRetries(t *testing.T) {
	refusal := apierrors.NewInvalid(schema.GroupKind{Group: "swarmada.io", Kind: "Robot"}, "dr-1", field.ErrorList{
		field.Invalid(field.NewPath("spec", "adapter", "name"), "acme-adapter",
			`FleetAdapter "acme-adapter" is not ready: phase=Disconnected conformance=Passed`),
	})
	r, rec, _ := newRefusingDRReconciler(t, refusal, marked(t, "dr-1", admission.Params{Zone: "z", Adapter: "acme-adapter"}))

	if err := reconcileDRErr(r, "dr-1"); err == nil {
		t.Fatal("a refusal that can clear must be retried, not dropped")
	}
	events := drainDREvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning AdmissionFailed") || !strings.Contains(events[0], "is not ready") {
		t.Fatalf("want one Warning AdmissionFailed carrying the refusal, got %q", events)
	}
}

// The simulation adapter reports 0.1.0-sim, which the Robot CRD's pattern refuses. That refusal
// depends on the admission alone and is identical on every attempt, so the Robot is never sent:
// the admission is recorded as failed, and neither this reconcile nor the next retries it.
func TestOperatorAdmit_VersionTheCRDRefusesIsNeverSent(t *testing.T) {
	dr := marked(t, "sim-robot-001", admission.Params{Zone: "z", Adapter: "sim-fleet-adapter"})
	dr.Status.AdapterVersion = "0.1.0-sim"
	r, rec, creates := newRefusingDRReconciler(t, nil, dr)

	for i := 0; i < 3; i++ {
		if err := reconcileDRErr(r, "sim-robot-001"); err != nil {
			t.Fatalf("reconcile %d: a refusal that cannot clear must not be retried with backoff: %v", i, err)
		}
	}
	if *creates != 0 {
		t.Errorf("a Robot the CRD must refuse was sent %d times", *creates)
	}
	events := drainDREvents(rec)
	if len(events) == 0 || !strings.HasPrefix(events[0], "Warning AdmissionFailed") || !strings.Contains(events[0], "spec.adapter.version") {
		t.Fatalf("want a Warning AdmissionFailed naming spec.adapter.version, got %q", events)
	}
}

// Auto-admit builds through the same builder, so a pre-release the class does not replace is
// skipped rather than sent: the robot stays for operator admission, and nothing is created.
func TestAutoAdmit_VersionTheCRDRefusesIsSkippedNotSent(t *testing.T) {
	dr := aaDiscovered("sim-robot-001", "picker-v2")
	dr.Status.AdapterVersion = "0.1.0-sim"
	r, _, creates := newRefusingDRReconciler(t, nil, dr, aaClass("picker-v2"), aaZone("zone-a"), aaConfig("picker-v2", "zone-a"))

	if err := reconcileDRErr(r, "sim-robot-001"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if *creates != 0 {
		t.Errorf("a Robot the CRD must refuse was sent %d times", *creates)
	}
	if getRobotOrNil(t, r, "sim-robot-001") != nil {
		t.Error("no Robot may be auto-admitted with a version the CRD refuses")
	}
}
