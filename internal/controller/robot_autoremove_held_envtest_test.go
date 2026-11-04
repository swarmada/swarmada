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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Offline auto-removal (ADR-0030) deletes an auto-admitted robot that has been Offline
// past the grace with no live lease, and auto-admit creates it again when it announces
// itself. A robot's stop lives only in its status, so the robot that came back was Normal
// and dispatchable: an estop, powered off, back on after the grace, and released with no
// operator (RFC-0001 #safety-estop-state-machine). Auto-removal now leaves a robot that
// holds a stop where it is, and says so in an Event.
func TestEnvtest_AutoRemovalLeavesAHeldOfflineRobotInPlace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, configWithSpec(ns, fleetv1.SwarmadaConfigSpec{
		Provisioning: fleetv1.SwarmadaProvisioningConfig{
			AutoRemoveOfflineRobots:       true,
			AutoRemoveOfflineGraceSeconds: 60,
		},
	})); err != nil {
		t.Fatalf("creating the SwarmadaConfig: %v", err)
	}

	cases := []struct {
		name string
		held string // what the Event must name; "" for the control, which is removed
		seed func(*fleetv1.RobotStatus)
	}{
		{"stopped", "estopState Stopped", func(s *fleetv1.RobotStatus) { s.EstopState = fleetv1.RobotEstopStopped }},
		{"resuming", "estopState Resuming", func(s *fleetv1.RobotStatus) { s.EstopState = fleetv1.RobotEstopResuming }},
		{"failed-lidar", "Failed component lidar", func(s *fleetv1.RobotStatus) {
			s.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareFailed}}
		}},
		{"released", "", func(s *fleetv1.RobotStatus) { s.EstopState = fleetv1.RobotEstopNormal }},
	}
	offlineSince := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	lastSeen := metav1.NewTime(time.Now().Add(-time.Hour))
	for _, c := range cases {
		r := envtestValidRobot(ns, c.name, "z-a")
		r.Annotations[fleetv1.AutoAdmittedAnnotation] = "true"
		if err := envK8s.Create(ctx, r); err != nil {
			t.Fatalf("creating %s: %v", c.name, err)
		}
		r.Status.Phase = fleetv1.RobotPhaseOffline
		r.Status.OfflineSince = &offlineSince
		r.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &lastSeen}
		c.seed(&r.Status)
		if err := envK8s.Status().Update(ctx, r); err != nil {
			t.Fatalf("seeding %s: %v", c.name, err)
		}
	}

	rec := record.NewFakeRecorder(32)
	robots := &RobotReconciler{Client: envK8s, Scheme: envScheme, Recorder: rec}
	reconcile := func(name string) {
		t.Helper()
		if _, err := robots.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
			t.Fatalf("reconciling %s: %v", name, err)
		}
	}
	// Twice each: a robot left in place is reported once, not on every pass.
	for _, c := range cases {
		reconcile(c.name)
		reconcile(c.name)
	}
	var events []string
	for len(rec.Events) > 0 {
		events = append(events, <-rec.Events)
	}

	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got fleetv1.Robot
			err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: c.name}, &got)
			if c.held == "" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("the control robot, which holds no stop, was not auto-removed (err=%v); the test proves nothing", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("robot %s, which holds %s, was auto-removed: %v", c.name, c.held, err)
			}
			if scheduler.ExcludedByForAssignment(&got, action, false) == nil {
				t.Errorf("robot %s is dispatchable after the auto-removal pass", c.name)
			}
			var mine []string
			for _, e := range events {
				if strings.Contains(e, c.held) {
					mine = append(mine, e)
				}
			}
			if len(mine) != 1 {
				t.Fatalf("want exactly one Event naming %q for %s, got %d: %v", c.held, c.name, len(mine), events)
			}
			for _, want := range []string{"Warning", "AutoRemovalHeld", "swarmctl estop resume"} {
				if !strings.Contains(mine[0], want) {
					t.Errorf("the Event does not say %q: %s", want, mine[0])
				}
			}
		})
	}
}
