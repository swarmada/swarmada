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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

func offlineTestRobot(ns, name string, phase fleetv1.RobotPhase, lastSeen *metav1.Time, offlineSince *metav1.Time) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: fleetv1.RobotStatus{
			Phase:        phase,
			OfflineSince: offlineSince,
			Connectivity: &fleetv1.ConnectivityStatus{LastSeenAt: lastSeen},
		},
	}
}

func reconcileRobotFor(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: objs[0].GetName(), Namespace: objs[0].GetNamespace()},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

// offlineDurationSampleCount gathers the histogram and returns the observation
// count for a namespace (ToFloat64 does not work on histograms).
func offlineDurationSampleCount(t *testing.T, ns string) uint64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.RobotOfflineDurationSeconds)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "swarmada_robot_offline_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "namespace" && lp.GetValue() == ns {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

// Entering Offline (heartbeat timeout) anchors OfflineSince.
func TestRobotOfflineMetrics_AnchorsOnOffline(t *testing.T) {
	stale := &metav1.Time{Time: time.Now().Add(-60 * time.Second)} // age > 30s heartbeatTimeout
	c := reconcileRobotFor(t, offlineTestRobot("off-anchor-ns", "r1", fleetv1.RobotPhaseIdle, stale, nil))

	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: "off-anchor-ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseOffline {
		t.Fatalf("phase = %s, want Offline", got.Status.Phase)
	}
	if got.Status.OfflineSince == nil {
		t.Error("OfflineSince should be anchored when the robot goes Offline")
	}
}

// Reconnect (phase left Offline while OfflineSince set) observes the span and
// clears the anchor — fired exactly once.
func TestRobotOfflineMetrics_ObservesAndClearsOnReconnect(t *testing.T) {
	ns := "off-observe-ns"
	recent := &metav1.Time{Time: time.Now()} // fresh heartbeat → no re-timeout
	offlineSince := &metav1.Time{Time: time.Now().Add(-90 * time.Second)}
	before := offlineDurationSampleCount(t, ns)

	c := reconcileRobotFor(t, offlineTestRobot(ns, "r1", fleetv1.RobotPhaseIdle, recent, offlineSince))

	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: ns}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.OfflineSince != nil {
		t.Error("OfflineSince should be cleared on reconnect")
	}
	if got := offlineDurationSampleCount(t, ns) - before; got != 1 {
		t.Errorf("offline_duration observations delta = %d, want 1", got)
	}
}
