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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// fakeResumer records the confirmation requests it is handed and replies with a
// scripted state and reason.
type fakeResumer struct {
	reply    fleetv1.RobotEstopState
	why      string
	err      error
	attempts []string
}

func (f *fakeResumer) ConfirmResume(_ context.Context, _, robotID, by string) (fleetv1.RobotEstopState, string, error) {
	f.attempts = append(f.attempts, robotID+" by "+by)
	return f.reply, f.why, f.err
}

// resumingRobot is a robot cleared into Resuming, optionally carrying an operator's
// release request.
func resumingRobot(t *testing.T, res ResumeConfirmer, request string) (*RobotEstopReconciler, *record.FakeRecorder, func() *fleetv1.Robot) {
	t.Helper()
	robot := robotWithEstop("amr-1", "", "")
	if request != "" {
		robot.Annotations = map[string]string{
			fleetv1.EstopResumeAnnotation: request,
			"swarmada.io/estop-actor":     "alice",
		}
	}
	r, c := newRobotEstop(t, &fakeEstopper{}, robot)
	r.Resumer = res
	rec := record.NewFakeRecorder(8)
	r.Recorder = rec
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	if err := c.Status().Update(context.Background(), robot); err != nil {
		t.Fatal(err)
	}
	get := func() *fleetv1.Robot {
		got := &fleetv1.Robot{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: zeNS, Name: "amr-1"}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	return r, rec, get
}

func reconcileResume(t *testing.T, r *RobotEstopReconciler) ctrl.Result {
	t.Helper()
	out, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "amr-1"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return out
}

// A Resuming robot that nobody has asked to release is left alone: the dispatcher is
// not consulted, and nothing is scheduled to look at it again. There is no automatic
// release, so there is nothing to poll for.
func TestRobotEstopResume_NoRequestNoRelease(t *testing.T) {
	res := &fakeResumer{reply: fleetv1.RobotEstopNormal}
	r, _, get := resumingRobot(t, res, "")
	out := reconcileResume(t, r)
	if len(res.attempts) != 0 {
		t.Fatalf("ConfirmResume called %d times with no operator request, want 0", len(res.attempts))
	}
	if out.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want 0", out.RequeueAfter)
	}
	if got := get().Status.EstopState; got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming", got)
	}
}

// An operator's request is handed to the dispatcher once, attributed to the operator
// the admission webhook stamped; it is marked handled and the operator is told.
func TestRobotEstopResume_RequestIsRelayedOnce(t *testing.T) {
	res := &fakeResumer{reply: fleetv1.RobotEstopNormal, why: "1 capabilities Active"}
	r, rec, get := resumingRobot(t, res, "2026-09-27T20:00:00Z inspected")
	reconcileResume(t, r)
	reconcileResume(t, r)
	if len(res.attempts) != 1 || res.attempts[0] != "amr-1 by alice" {
		t.Fatalf("attempts = %v, want exactly one, by alice", res.attempts)
	}
	got := get()
	if got.Annotations[fleetv1.EstopResumeProcessedAnnotation] != "2026-09-27T20:00:00Z inspected" {
		t.Fatalf("processed marker = %q", got.Annotations[fleetv1.EstopResumeProcessedAnnotation])
	}
	if _, left := got.Annotations["swarmada.io/estop-actor"]; left {
		t.Fatal("the operator stamp was not consumed with the request")
	}
	if e := <-rec.Events; !strings.HasPrefix(e, "Normal EstopResumeConfirmed") {
		t.Fatalf("event %q, want EstopResumeConfirmed", e)
	}
}

// With no confirmer wired, or when the request cannot be evaluated, the request is
// answered as refused and closed. It is not retried: a retry would release the robot
// at a moment nobody chose.
func TestRobotEstopResume_UnanswerableRequestIsRefusedNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  ResumeConfirmer
		want string
	}{
		{"no confirmer", nil, "not configured"},
		{"evaluation error", &fakeResumer{err: errors.New("apiserver unavailable")}, "apiserver unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rec, get := resumingRobot(t, tc.res, "inspected")
			if tc.res == nil {
				r.Resumer = nil
			}
			out := reconcileResume(t, r)
			if out.RequeueAfter != 0 {
				t.Fatalf("RequeueAfter = %v, want 0", out.RequeueAfter)
			}
			got := get()
			if got.Status.EstopState != fleetv1.RobotEstopResuming {
				t.Fatalf("estopState = %s, want Resuming", got.Status.EstopState)
			}
			if got.Annotations[fleetv1.EstopResumeProcessedAnnotation] != "inspected" {
				t.Fatal("the request was left pending, to be evaluated again later")
			}
			if e := <-rec.Events; !strings.HasPrefix(e, "Warning EstopResumeRefused") || !strings.Contains(e, tc.want) {
				t.Fatalf("event %q, want EstopResumeRefused naming %q", e, tc.want)
			}
		})
	}
}

// A new trigger on a robot in Resuming is issued: re-enable never overrides a hazard
// raised during re-enable (RFC-0001 #safety-estop-state-machine).
func TestRobotEstopResume_TriggerDuringResumingFires(t *testing.T) {
	est := &fakeEstopper{}
	robot := robotWithEstop("amr-1", "second hazard", "")
	r, c := newRobotEstop(t, est, robot)
	r.Resumer = &fakeResumer{}
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	if err := c.Status().Update(context.Background(), robot); err != nil {
		t.Fatal(err)
	}
	reconcileResume(t, r)
	if len(est.estopped) != 1 {
		t.Fatalf("estop issued %d times on a trigger during Resuming, want 1", len(est.estopped))
	}
}
