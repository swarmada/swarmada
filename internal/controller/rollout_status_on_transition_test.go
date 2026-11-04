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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A rollout's status is written on a transition, not on every reconcile (RA-1). A
// rollout watches Robots, so a busy fleet reconciles it often; a pass that changes
// nothing must write nothing, and a condition whose status did not change keeps the
// lastTransitionTime it had (metav1.Condition: "the last time the condition
// transitioned from one status to another").
//
// The writes are counted at the client: a merge patch identical to the stored object
// leaves resourceVersion alone on a real API server, so resourceVersion cannot tell a
// needless write from none.

// countRolloutStatusPatches wraps c so every status patch of a rollout of type T is counted.
func countRolloutStatusPatches[T client.Object](c client.WithWatch) (client.WithWatch, *atomic.Int64) {
	var n atomic.Int64
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if _, ok := obj.(T); ok && sub == "status" {
				n.Add(1)
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}), &n
}

func conditionTimes(conds []metav1.Condition) map[string]metav1.Time {
	out := make(map[string]metav1.Time, len(conds))
	for _, c := range conds {
		out[c.Type] = c.LastTransitionTime
	}
	return out
}

// quietReconciles runs passes with nothing changed in between, a second apart so a
// re-stamped lastTransitionTime would show at the API's one-second resolution, and
// fails if any of them wrote status or moved a condition's lastTransitionTime.
func quietReconciles(t *testing.T, passes int, writes *atomic.Int64, reconcile func(),
	conds func() []metav1.Condition) {
	t.Helper()
	before, times := writes.Load(), conditionTimes(conds())
	if len(times) == 0 {
		t.Fatal("the establishing pass wrote no condition")
	}
	for i := 0; i < passes; i++ {
		time.Sleep(1100 * time.Millisecond)
		reconcile()
	}
	if got := writes.Load() - before; got != 0 {
		t.Errorf("%d reconciles with nothing changed wrote status %d time(s), want 0", passes, got)
	}
	for typ, at := range conditionTimes(conds()) {
		if prev, ok := times[typ]; !ok || !prev.Equal(&at) {
			t.Errorf("condition %s lastTransitionTime moved %v -> %v with its status unchanged", typ, prev, at)
		}
	}
}

func holdRollout(t *testing.T, c client.Client, obj client.Object, set func()) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	set()
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatalf("hold rollout: %v", err)
	}
}

func TestFirmwareRollout_StatusWrittenOnlyOnATransition(t *testing.T) {
	for _, enforced := range []bool{true, false} {
		t.Run(map[bool]string{true: "signing enforced", false: "signing not enforced"}[enforced], func(t *testing.T) {
			secret, sigRef := signingFixture(t, fwChecksum)
			scheme := runtime.NewScheme()
			if err := fleetv1.AddToScheme(scheme); err != nil {
				t.Fatalf("scheme: %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("corev1 scheme: %v", err)
			}
			c, writes := countRolloutStatusPatches[*fleetv1.FirmwareRollout](fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(fwRollout(sigRef), signingConfig(enforced), secret,
					targetRobot("r1", fleetv1.RobotPhaseIdle, 90)).
				WithStatusSubresource(&fleetv1.FirmwareRollout{}, &fleetv1.Robot{}).
				Build())
			r := &FirmwareRolloutReconciler{Client: c, Scheme: scheme}

			reconcileFirmware(t, r) // establishes the batch and the conditions
			conds := func() []metav1.Condition { return getFirmwareRollout(t, c).Status.Conditions }
			quietReconciles(t, 2, writes, func() { reconcileFirmware(t, r) }, conds)

			// A transition is still written, once, and only the condition that
			// transitioned gets a new lastTransitionTime.
			times := conditionTimes(conds())
			fw := &fleetv1.FirmwareRollout{ObjectMeta: metav1.ObjectMeta{Name: "fw", Namespace: rolloutNS}}
			holdRollout(t, c, fw, func() { fw.Spec.DesiredState = fleetv1.RolloutDesiredStatePaused })
			time.Sleep(1100 * time.Millisecond)
			before := writes.Load()
			reconcileFirmware(t, r)
			if got := writes.Load() - before; got != 1 {
				t.Fatalf("an operator hold wrote status %d time(s), want 1", got)
			}
			after := conditionTimes(conds())
			if at, prev := after[conditionOperatorHold], times[conditionOperatorHold]; at.Equal(&prev) {
				t.Errorf("OperatorHold went True and its lastTransitionTime did not move (%v)", at)
			}
			if at, prev := after[conditionSignatureVerified], times[conditionSignatureVerified]; !at.Equal(&prev) {
				t.Errorf("SignatureVerified did not transition and its lastTransitionTime moved %v -> %v", prev, at)
			}
		})
	}
}

// The ModelRollout controller assembles its status the same way, so it is held to
// the same rule.
func TestModelRollout_StatusWrittenOnlyOnATransition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, writes := countRolloutStatusPatches[*fleetv1.ModelRollout](fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90)).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).
		Build())
	r := &ModelRolloutReconciler{Client: c, Scheme: scheme}

	reconcileRollout(t, r)
	conds := func() []metav1.Condition { return getModelRollout(t, c).Status.Conditions }
	quietReconciles(t, 2, writes, func() { reconcileRollout(t, r) }, conds)

	times := conditionTimes(conds())
	mr := &fleetv1.ModelRollout{ObjectMeta: metav1.ObjectMeta{Name: "roll", Namespace: rolloutNS}}
	holdRollout(t, c, mr, func() { mr.Spec.DesiredState = fleetv1.RolloutDesiredStatePaused })
	time.Sleep(1100 * time.Millisecond)
	before := writes.Load()
	reconcileRollout(t, r)
	if got := writes.Load() - before; got != 1 {
		t.Fatalf("an operator hold wrote status %d time(s), want 1", got)
	}
	if at, prev := conditionTimes(conds())[conditionOperatorHold], times[conditionOperatorHold]; at.Equal(&prev) {
		t.Errorf("OperatorHold went True and its lastTransitionTime did not move (%v)", at)
	}
}

// The same rule against a real API server: the stored object is what an operator
// reads, and its round trip is what the equality check compares against.
func TestEnvtest_FirmwareRollout_StatusWrittenOnlyOnATransition(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	ctx := context.Background()

	base, err := client.NewWithWatch(envCfg, client.Options{Scheme: envScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	c, writes := countRolloutStatusPatches[*fleetv1.FirmwareRollout](base)

	cfg := signingConfig(false)
	cfg.Namespace = ns
	rollout := fwRollout("")
	rollout.Namespace = ns
	fixture := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	robot := envtestValidRobot(ns, "r1", "z-a")
	robot.Labels = fixture.Labels
	robot.Spec.Hardware = fixture.Spec.Hardware
	status := fixture.Status
	for _, obj := range []client.Object{cfg, rollout, robot} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	robot.Status = status
	if err := c.Status().Update(ctx, robot); err != nil {
		t.Fatalf("robot status: %v", err)
	}

	r := &FirmwareRolloutReconciler{Client: c, Scheme: envScheme}
	key := types.NamespacedName{Name: rollout.Name, Namespace: ns}
	reconcile := func() {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	conds := func() []metav1.Condition {
		fw := &fleetv1.FirmwareRollout{}
		if err := c.Get(ctx, key, fw); err != nil {
			t.Fatalf("get rollout: %v", err)
		}
		return fw.Status.Conditions
	}
	reconcile()
	quietReconciles(t, 2, writes, reconcile, conds)
}
