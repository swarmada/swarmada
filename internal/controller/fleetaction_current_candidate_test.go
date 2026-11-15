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

// The robot dispatch selects comes from the cache. Each test here gives the reconciler a cache
// that has not yet seen the robot's latest write (a second fake), reading from it and writing to
// the API server's fake, as the manager's client does.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

type candidateFixture struct {
	r          *FleetActionReconciler
	api, cache client.Client
	tde        *fakeTDE
}

// newCandidateFixture seeds the cache and the API server with the same objects, at the same
// versions, then applies live (when not nil) to the API server's copy of robot r1 only.
func newCandidateFixture(t *testing.T, live func(*fleetv1.Robot), objs ...client.Object) *candidateFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	build := func() client.Client {
		seed := []client.Object{readyActionAdapter()}
		for _, o := range objs {
			seed = append(seed, o.DeepCopyObject().(client.Object))
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).
			WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	}
	s := &candidateFixture{api: build(), cache: build(), tde: &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}}
	if live != nil {
		ctx := context.Background()
		var robot fleetv1.Robot
		if err := s.api.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, &robot); err != nil {
			t.Fatal(err)
		}
		// live may change metadata and status; each write keeps only its own half, so live is
		// applied before each.
		live(&robot)
		if err := s.api.Update(ctx, &robot); err != nil {
			t.Fatalf("update the live robot: %v", err)
		}
		live(&robot)
		if err := s.api.Status().Update(ctx, &robot); err != nil {
			t.Fatalf("update the live robot's status: %v", err)
		}
	}
	s.r = &FleetActionReconciler{Client: cachedClient{Client: s.api, cache: s.cache}, APIReader: s.api, Scheme: scheme,
		Scheduler: scheduler.NewDefaultScheduler(), TDE: s.tde}
	return s
}

func (s *candidateFixture) reconcile(name string) error {
	_, err := s.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: name}})
	return err
}

func waitingSince(a *fleetv1.FleetAction, d time.Duration) *fleetv1.FleetAction {
	a.Status.PendingSince = &metav1.Time{Time: time.Now().Add(-d)}
	return a
}

// A robot another action took after the cache's read is not given this action: the reconcile
// asks no TDE slot for it, writes no assignment and reverts nothing, observes no latency, and
// says the robot is not Idle, which is what the next recheck, on a current cache, also says.
func TestCurrentCandidate_ARobotTakenSinceTheCacheReadIsNotSelected(t *testing.T) {
	s := newCandidateFixture(t, func(r *fleetv1.Robot) {
		r.Status.Phase, r.Status.AssignedAction = fleetv1.RobotPhaseInProgress, "t0"
	}, waitingSince(zonedAction("t1", "z"), 30*time.Second), idleRobotInZone("r1", "z"))
	before, _ := assignLatency(t, actionNS, "Normal")

	err := s.reconcile("t1")

	a := getAction(t, s.api, "t1")
	if s.tde.requests != 0 {
		t.Errorf("a TDE slot was asked %d times for a robot already taken", s.tde.requests)
	}
	if a.Status.AssignedRobot != "" || a.Status.Phase != fleetv1.ActionPhasePending {
		t.Errorf("the action is %s on %q, want Pending on no robot", a.Status.Phase, a.Status.AssignedRobot)
	}
	if strings.Contains(a.Status.Message, "robot commit failed") {
		t.Errorf("the action was assigned and reverted: %q", a.Status.Message)
	}
	if !strings.Contains(a.Status.Message, "1 not Idle (filter 1)") || strings.Contains(a.Status.Message, "no adapter would serve") {
		t.Errorf("the message does not say the robot is not Idle: %q", a.Status.Message)
	}
	if err != nil {
		t.Errorf("reconcile: %v (a lost robot commit returns an error and requeues rate-limited)", err)
	}
	if after, _ := assignLatency(t, actionNS, "Normal"); after != before {
		t.Errorf("%d assignment-latency samples observed for no assignment", after-before)
	}
	var robot fleetv1.Robot
	if err := s.api.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "r1"}, &robot); err != nil {
		t.Fatal(err)
	}
	if robot.Status.AssignedAction != "t0" {
		t.Errorf("the robot now records %q, want t0", robot.Status.AssignedAction)
	}
}

// A robot still free but written after the cache's read (a telemetry write) is assigned on its
// live copy: the robot commit lands at once, and one latency sample is observed, after it.
func TestCurrentCandidate_ARobotStillFreeIsAssignedOnItsLiveCopy(t *testing.T) {
	s := newCandidateFixture(t, func(r *fleetv1.Robot) {
		r.Status.Conditions = []metav1.Condition{{Type: "Telemetry", Status: metav1.ConditionTrue, Reason: "Fresh",
			LastTransitionTime: metav1.Now()}}
	}, waitingSince(zonedAction("t1", "z"), 30*time.Second), idleRobotInZone("r1", "z"))
	before, _ := assignLatency(t, actionNS, "Normal")

	if err := s.reconcile("t1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	a := getAction(t, s.api, "t1")
	if a.Status.AssignedRobot != "r1" || a.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("the action is %s on %q (%q), want Assigned on r1", a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
	}
	if after, _ := assignLatency(t, actionNS, "Normal"); after-before != 1 {
		t.Errorf("%d assignment-latency samples, want 1", after-before)
	}
}

// A robot commit that loses its lock after the action was written reverts the action to the
// wait it was in: no latency sample is observed (it is taken only once the robot commit has
// landed), and the action's PendingSince is the one it had, not the moment of the revert.
func TestCurrentCandidate_ALostRobotCommitKeepsTheWaitAndObservesNoLatency(t *testing.T) {
	waiting := waitingSince(zonedAction("t1", "z"), 30*time.Second)
	since := waiting.Status.PendingSince.Time
	s := newCandidateFixture(t, nil, waiting, idleRobotInZone("r1", "z"))
	lost := interceptor.NewClient(s.api.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*fleetv1.Robot); ok && sub == "status" {
				return apierrors.NewConflict(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "robots"},
					obj.GetName(), errors.New("written since it was read"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	s.r.Client = cachedClient{Client: lost, cache: s.cache}
	before, _ := assignLatency(t, actionNS, "Normal")

	_ = s.reconcile("t1")

	a := getAction(t, s.api, "t1")
	if a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" ||
		!strings.Contains(a.Status.Message, "robot commit failed") {
		t.Fatalf("the action is %s on %q (%q), want reverted to Pending after the lost robot commit",
			a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
	}
	if after, _ := assignLatency(t, actionNS, "Normal"); after != before {
		t.Errorf("%d assignment-latency samples observed for an assignment that lost its robot", after-before)
	}
	if a.Status.PendingSince == nil || a.Status.PendingSince.Time.Sub(since).Abs() > time.Second {
		t.Errorf("PendingSince is %v after the revert, want the original %v", a.Status.PendingSince, since)
	}
}

// What the re-read decides, case by case.
func TestCurrentCandidate_Decisions(t *testing.T) {
	needsLift := zonedAction("t1", "z")
	needsLift.Spec.RequiredCapabilities = []string{"lift"}
	liftRobot := idleRobotInZone("r1", "z")
	liftRobot.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "lift", Status: fleetv1.CapabilityStatusActive}}

	for _, tc := range []struct {
		name       string
		live       func(*fleetv1.Robot)
		getErr     error
		wantFilter *int // nil: current
		wantLive   bool // the live copy is returned, not the cache's
	}{
		{"unchanged since the cache read", nil, nil, nil, false},
		{"telemetry written, still free and qualified", func(r *fleetv1.Robot) {
			r.Status.Conditions = []metav1.Condition{{Type: "Telemetry", Status: metav1.ConditionTrue, Reason: "Fresh",
				LastTransitionTime: metav1.Now()}}
		}, nil, nil, true},
		{"taken by another action", func(r *fleetv1.Robot) {
			r.Status.Phase, r.Status.AssignedAction = fleetv1.RobotPhaseInProgress, "t0"
		}, nil, ptrInt(scheduler.FilterPhaseIdle), false},
		{"Idle, but still recording another action", func(r *fleetv1.Robot) {
			r.Status.AssignedAction = "t0"
		}, nil, ptrInt(scheduler.FilterPhaseIdle), false},
		{"a zone's stop now holds it", func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}
		}, nil, ptrInt(filterEstop), false},
		{"its capability is lost (the scheduler's filters)", func(r *fleetv1.Robot) {
			r.Status.Capabilities[0].Status = fleetv1.CapabilityStatusInactive
		}, nil, ptrInt(scheduler.FilterRequiredCapability), false},
		{"it moved out of the action's zone (the pass's cross-object filters)", func(r *fleetv1.Robot) {
			r.Status.CurrentZone = "elsewhere"
		}, nil, ptrInt(filterZoneLeafSet), false},
		{"its wire robot-id changed (the pass's robot-id hold keys on the old one)", func(r *fleetv1.Robot) {
			r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: "another-robots-id"}
		}, nil, ptrInt(filterRobotIDConflict), false},
		{"it was deleted", nil, errNotFoundRobot, ptrInt(scheduler.FilterTerminating), false},
		{"the re-read fails: the cache's copy, and the robot commit's lock decides", nil, errors.New("timeout"), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCandidateFixture(t, tc.live, needsLift, liftRobot)
			if tc.getErr != nil {
				s.r.APIReader = failingRobotGet(s.api, tc.getErr)
			}
			ctx := context.Background()
			var cached fleetv1.Robot
			if err := s.cache.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, &cached); err != nil {
				t.Fatal(err)
			}
			judge, _, _ := s.r.newDispatchJudge(ctx, needsLift)
			got, ex := s.r.currentCandidate(ctx, needsLift, &cached, judge, false)
			switch {
			case tc.wantFilter == nil && ex != nil:
				t.Fatalf("withheld by filter %d (%s), want current", ex.Filter, ex.Reason)
			case tc.wantFilter != nil && ex == nil:
				t.Fatalf("current, want withheld by filter %d", *tc.wantFilter)
			case tc.wantFilter != nil:
				if ex.Filter != *tc.wantFilter || ex.Robot != "r1" {
					t.Fatalf("withheld %s by filter %d (%s), want r1 by filter %d", ex.Robot, ex.Filter, ex.Reason, *tc.wantFilter)
				}
				return
			}
			if isLive := got.ResourceVersion != cached.ResourceVersion; isLive != tc.wantLive {
				t.Errorf("returned the live copy: %v, want %v", isLive, tc.wantLive)
			}
		})
	}
}

func ptrInt(i int) *int { return &i }

var errNotFoundRobot = errors.New("not found")

// failingRobotGet is the API server's fake with every Robot Get failing: NotFound for
// errNotFoundRobot, err otherwise.
func failingRobotGet(api client.Client, err error) client.Reader {
	return interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*fleetv1.Robot); ok {
				if errors.Is(err, errNotFoundRobot) {
					return apierrors.NewNotFound(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "robots"}, key.Name)
				}
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}
