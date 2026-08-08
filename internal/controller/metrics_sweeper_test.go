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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

func sweeperClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func swAction(ns, name string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

func swRobot(ns, name string, phase fleetv1.RobotPhase, estop fleetv1.RobotEstopState) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     fleetv1.RobotStatus{Phase: phase, EstopState: estop},
	}
}

func swAdapter(ns, name string, phase fleetv1.FleetAdapterPhase) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     fleetv1.FleetAdapterStatus{Phase: phase},
	}
}

// One sweep sets every aggregate gauge from the live resources — including 0 for
// enum values with no resources (§9.3.8 registration requirement).
func TestMetricsSweeper_SetsGaugesFromResources(t *testing.T) {
	ns := "sweep-ns"
	c := sweeperClient(t,
		swAction(ns, "t1", fleetv1.ActionPhaseRevoking),
		swAction(ns, "t2", fleetv1.ActionPhasePending),
		swRobot(ns, "r1", fleetv1.RobotPhaseIdle, fleetv1.RobotEstopNormal),
		swRobot(ns, "r2", fleetv1.RobotPhaseOffline, fleetv1.RobotEstopStopped),
		swRobot(ns, "r3", fleetv1.RobotPhaseInProgress, fleetv1.RobotEstopStopping),
		swAdapter(ns, "a-conn", fleetv1.FleetAdapterPhaseConnected),
		swAdapter(ns, "a-disc", fleetv1.FleetAdapterPhaseDisconnected),
	)
	s := &MetricsSweeper{Client: c}
	s.sweep(context.Background())

	check := func(name string, g prometheus.Gauge, want float64) {
		if got := testutil.ToFloat64(g); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("fleetactions{Revoking}", metrics.FleetActionsByPhase.WithLabelValues(ns, "Revoking"), 1)
	check("fleetactions{Pending}", metrics.FleetActionsByPhase.WithLabelValues(ns, "Pending"), 1)
	check("fleetactions{Succeeded}=0", metrics.FleetActionsByPhase.WithLabelValues(ns, "Succeeded"), 0)

	check("robots{Idle}", metrics.RobotsByPhase.WithLabelValues(ns, "Idle"), 1)
	check("robots{Offline}", metrics.RobotsByPhase.WithLabelValues(ns, "Offline"), 1)
	check("robots{InProgress}", metrics.RobotsByPhase.WithLabelValues(ns, "InProgress"), 1)
	check("robots{Charging}=0", metrics.RobotsByPhase.WithLabelValues(ns, "Charging"), 0)

	check("estop{Stopped}", metrics.RobotsInEstop.WithLabelValues(ns, "Stopped"), 1)
	check("estop{Stopping}", metrics.RobotsInEstop.WithLabelValues(ns, "Stopping"), 1)

	check("adapter a-conn connected=1", metrics.FleetAdapterConnected.WithLabelValues(ns, "a-conn"), 1)
	check("adapter a-disc connected=0", metrics.FleetAdapterConnected.WithLabelValues(ns, "a-disc"), 0)
	check("adapter a-conn phase{Connected}=1", metrics.FleetAdapterPhase.WithLabelValues(ns, "a-conn", "Connected"), 1)
	check("adapter a-conn phase{Disconnected}=0", metrics.FleetAdapterPhase.WithLabelValues(ns, "a-conn", "Disconnected"), 0)
}

// adapterConnectedSeries reports whether a swarmada_fleet_adapter_connected series
// exists for (ns, adapter) — used to prove de-staling removes it (ToFloat64 would
// recreate the series, defeating the check).
func adapterConnectedSeries(t *testing.T, ns, adapter string) bool {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.FleetAdapterConnected)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "swarmada_fleet_adapter_connected" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["namespace"] == ns && l["adapter"] == adapter {
				return true
			}
		}
	}
	return false
}

// A deleted adapter must not keep reporting connected — its series is de-staled on
// the next sweep (safety-relevant: a phantom connected=1 hides an estop-path outage).
func TestMetricsSweeper_DeStalesDeletedAdapter(t *testing.T) {
	ns := "destale-ns"
	s := &MetricsSweeper{Client: sweeperClient(t, swAdapter(ns, "gone", fleetv1.FleetAdapterPhaseConnected))}
	s.sweep(context.Background())
	if !adapterConnectedSeries(t, ns, "gone") {
		t.Fatal("adapter series should exist after the first sweep")
	}

	// Adapter deleted: re-point the sweeper at an empty cluster and sweep again.
	s.Client = sweeperClient(t)
	s.sweep(context.Background())
	if adapterConnectedSeries(t, ns, "gone") {
		t.Error("deleted adapter's connected series must be removed on the next sweep")
	}
}
