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
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// One resume, one ROLLOUT_RESUMED entry, even when the pass that applied it could not record
// it as processed. The retry finds the status already resumed, so it writes nothing and seals
// nothing before it records the marker.

// failMainPatchOnce builds a fake client whose next Patch of the rollout itself (not its
// status) fails once after arm() is called.
func failMainPatchOnce(t *testing.T, isRollout func(client.Object) bool, objs ...client.Object) (client.Client, func()) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{fleetv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	armed := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FirmwareRollout{}, &fleetv1.ModelRollout{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if armed && isRollout(obj) {
					armed = false
					return errors.New("simulated lost write")
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	return c, func() { armed = true }
}

func addResumeRequest(t *testing.T, c client.Client, obj client.Object, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	obj.SetAnnotations(map[string]string{rolloutResumeAnnotation: reason})
	if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
}

func TestRolloutResume_ALostMarkerWriteDoesNotSealASecondResume(t *testing.T) {
	t.Run("firmware", func(t *testing.T) {
		dispatched := time.Now().Add(-10 * time.Minute)
		secret, sigRef := signingFixture(t, fwChecksum)
		ro := pausingFwRollout(sigRef)
		seedBatch(ro, "r1", dispatched)
		c, arm := failMainPatchOnce(t, func(o client.Object) bool { _, ok := o.(*fleetv1.FirmwareRollout); return ok },
			ro, signingConfig(true), secret,
			failedFirmware("r1", "2.4.0", "flash verify failed", dispatched.Add(time.Minute)),
			targetRobot("r2", fleetv1.RobotPhaseIdle, 90))
		rec := &captureRecorder{}
		r := &FirmwareRolloutReconciler{Client: c, Scheme: c.Scheme(), Audit: rec}
		reconcileFirmware(t, r)
		if got := getFirmwareRollout(t, c).Status.Phase; got != fleetv1.RolloutPhasePaused {
			t.Fatalf("fixture did not pause: %s", got)
		}

		addResumeRequest(t, c, getFirmwareRollout(t, c), "r1 flash chip is dead")
		arm()
		key := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fw", Namespace: rolloutNS}}
		if _, err := r.Reconcile(context.Background(), key); err == nil {
			t.Fatal("fixture: the marker write was meant to fail on this pass")
		}
		for i := 0; i < 3; i++ {
			reconcileFirmware(t, r)
		}

		fw := getFirmwareRollout(t, c)
		if fw.Annotations[rolloutResumeProcessedAnnotation] != "r1 flash chip is dead" {
			t.Errorf("processed marker = %q, want the request", fw.Annotations[rolloutResumeProcessedAnnotation])
		}
		if n := len(entriesOfType(rec, audit.EventRolloutResumed)); n != 1 {
			t.Errorf("ROLLOUT_RESUMED entries = %d for one resume, want 1", n)
		}
	})

	t.Run("model", func(t *testing.T) {
		failed := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
		failed.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
			{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed"},
		}
		c, arm := failMainPatchOnce(t, func(o client.Object) bool { _, ok := o.(*fleetv1.ModelRollout); return ok },
			pickerRollout(), failed, targetRobot("r2", fleetv1.RobotPhaseIdle, 90))
		rec := &captureRecorder{}
		r := &ModelRolloutReconciler{Client: c, Scheme: c.Scheme(), Audit: rec}
		reconcileRollout(t, r)
		if got := getModelRollout(t, c).Status.Phase; got != fleetv1.RolloutPhasePaused {
			t.Fatalf("fixture did not pause: %s", got)
		}

		addResumeRequest(t, c, getModelRollout(t, c), "r1 has a failed SSD")
		arm()
		key := ctrl.Request{NamespacedName: types.NamespacedName{Name: "roll", Namespace: rolloutNS}}
		if _, err := r.Reconcile(context.Background(), key); err == nil {
			t.Fatal("fixture: the marker write was meant to fail on this pass")
		}
		for i := 0; i < 3; i++ {
			reconcileRollout(t, r)
		}

		mr := getModelRollout(t, c)
		if mr.Annotations[rolloutResumeProcessedAnnotation] != "r1 has a failed SSD" {
			t.Errorf("processed marker = %q, want the request", mr.Annotations[rolloutResumeProcessedAnnotation])
		}
		if n := len(entriesOfType(rec, audit.EventRolloutResumed)); n != 1 {
			t.Errorf("ROLLOUT_RESUMED entries = %d for one resume, want 1", n)
		}
	})
}
