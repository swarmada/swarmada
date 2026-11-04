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
	"fmt"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// resumeReads is what a reader of the Robot sees after each write the control plane
// makes while it answers a resume request: the processed marker and the estopState, as
// swarmctl estop resume reads them, one Get per write.
type resumeReads struct {
	mu    sync.Mutex
	reads []resumeRead
}

type resumeRead struct {
	after     string
	processed string
	state     fleetv1.RobotEstopState
}

func (o *resumeReads) list() []resumeRead {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]resumeRead(nil), o.reads...)
}

// observeResumeWrites wraps r's client so that every successful write to a Robot is
// followed by a read of the stored Robot.
func observeResumeWrites(r *RobotEstopReconciler) *resumeReads {
	o := &resumeReads{}
	inner := r.Client.(client.WithWatch)
	read := func(ctx context.Context, obj client.Object, after string) {
		if _, ok := obj.(*fleetv1.Robot); !ok {
			return
		}
		got := &fleetv1.Robot{}
		if err := inner.Get(ctx, client.ObjectKeyFromObject(obj), got); err != nil {
			return
		}
		o.mu.Lock()
		o.reads = append(o.reads, resumeRead{
			after:     after,
			processed: got.Annotations[fleetv1.EstopResumeProcessedAnnotation],
			state:     got.Status.EstopState,
		})
		o.mu.Unlock()
	}
	r.Client = interceptor.NewClient(inner, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			read(ctx, obj, "patch")
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}
			read(ctx, obj, "update")
			return nil
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := c.SubResource(sub).Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			read(ctx, obj, sub+" patch")
			return nil
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			read(ctx, obj, sub+" update")
			return nil
		},
	})
	return o
}

// writingResumer stands in for the dispatcher: it writes the outcome to robots/status
// through the control plane's client, as the single writer does, and returns it.
type writingResumer struct {
	c     client.Client
	reply fleetv1.RobotEstopState
	why   string
	err   error
	calls int
}

func (w *writingResumer) ConfirmResume(ctx context.Context, ns, name, _ string) (fleetv1.RobotEstopState, string, error) {
	w.calls++
	if w.err != nil {
		return "", "", w.err
	}
	if w.reply == fleetv1.RobotEstopNormal {
		robot := &fleetv1.Robot{}
		if err := w.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, robot); err != nil {
			return "", "", err
		}
		base := robot.DeepCopy()
		robot.Status.EstopState = fleetv1.RobotEstopNormal
		if err := w.c.Status().Patch(ctx, robot, client.MergeFrom(base)); err != nil {
			return "", "", err
		}
	}
	return w.reply, w.why, nil
}

// A reader that sees a resume request marked handled reads the estopState that answered
// it. swarmctl estop resume judges released or refused on the read where the marker
// appears; a marker written before the dispatcher's estopState write told the operator
// "refused" for a robot the control plane then released.
func TestRobotEstopResume_HandledMarkerComesAfterTheOutcome(t *testing.T) {
	const request = "2026-10-10T18:12:46Z drill: released"
	for _, tc := range []struct {
		name  string
		reply fleetv1.RobotEstopState
		err   error
		want  fleetv1.RobotEstopState
		event string
	}{
		{name: "released", reply: fleetv1.RobotEstopNormal, want: fleetv1.RobotEstopNormal, event: "Normal EstopResumeConfirmed"},
		{name: "refused", reply: fleetv1.RobotEstopResuming, want: fleetv1.RobotEstopResuming, event: "Warning EstopResumeRefused"},
		{name: "evaluation-error", err: errors.New("apiserver unavailable"), want: fleetv1.RobotEstopResuming, event: "Warning EstopResumeRefused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rec, get := resumingRobot(t, nil, request)
			reads := observeResumeWrites(r)
			res := &writingResumer{c: r.Client, reply: tc.reply, why: "rig", err: tc.err}
			r.Resumer = res

			reconcileResume(t, r)
			reconcileResume(t, r)

			var trace []string
			marked := 0
			for _, rd := range reads.list() {
				trace = append(trace, fmt.Sprintf("%s: processed=%q estopState=%s", rd.after, rd.processed, rd.state))
				if rd.processed != request {
					continue
				}
				marked++
				if rd.state != tc.want {
					t.Errorf("a reader saw the request handled with estopState %s; it was answered %s", rd.state, tc.want)
				}
			}
			t.Logf("RESULT reads after each write:\n  %s", strings.Join(trace, "\n  "))
			if marked == 0 {
				t.Errorf("no read showed the request handled")
			}
			if res.calls != 1 {
				t.Errorf("ConfirmResume called %d times, want 1", res.calls)
			}
			got := get()
			if got.Annotations[fleetv1.EstopResumeProcessedAnnotation] != request {
				t.Errorf("processed marker = %q, want the request", got.Annotations[fleetv1.EstopResumeProcessedAnnotation])
			}
			if _, left := got.Annotations[fleetv1.EstopResumeClaimedAnnotation]; left {
				t.Errorf("the claim was left on the Robot")
			}
			if got.Status.EstopState != tc.want {
				t.Errorf("estopState = %s, want %s", got.Status.EstopState, tc.want)
			}
			if e := <-rec.Events; !strings.HasPrefix(e, tc.event) {
				t.Errorf("event %q, want %s", e, tc.event)
			}
		})
	}
}

// A claim is never evaluated a second time, now that the claim alone (not the processed
// marker) is what stands while a request is evaluated: one the API server holds and the
// cached copy does not show yet is closed, and one left unanswered is closed as handled,
// so the request does not become pending again once the claim is gone.
func TestRobotEstopResume_AClaimIsClosedNotEvaluated(t *testing.T) {
	const request = "inspected"
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, r *RobotEstopReconciler, stale *fleetv1.Robot)
	}{
		{name: "the-cache-has-not-seen-the-claim", run: func(t *testing.T, r *RobotEstopReconciler, stale *fleetv1.Robot) {
			delete(stale.Annotations, fleetv1.EstopResumeClaimedAnnotation)
			if _, err := r.handleResumeRequest(context.Background(), stale, request); err != nil {
				t.Fatalf("handleResumeRequest: %v", err)
			}
			reconcileResume(t, r)
		}},
		{name: "a-claim-left-unanswered", run: func(t *testing.T, r *RobotEstopReconciler, _ *fleetv1.Robot) {
			reconcileResume(t, r)
			reconcileResume(t, r)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeResumer{reply: fleetv1.RobotEstopNormal}
			r, rec, get := resumingRobot(t, res, request)
			stored := get()
			base := stored.DeepCopy()
			stored.Annotations[fleetv1.EstopResumeClaimedAnnotation] = request
			if err := r.Patch(context.Background(), stored, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			tc.run(t, r, stored.DeepCopy())

			if len(res.attempts) != 0 {
				t.Errorf("a claimed request was evaluated %d times; it is closed, not evaluated", len(res.attempts))
			}
			got := get()
			if got.Annotations[fleetv1.EstopResumeProcessedAnnotation] != request {
				t.Errorf("processed marker = %q; the request would be pending again", got.Annotations[fleetv1.EstopResumeProcessedAnnotation])
			}
			if _, left := got.Annotations[fleetv1.EstopResumeClaimedAnnotation]; left {
				t.Errorf("the claim was not closed")
			}
			if got.Status.EstopState != fleetv1.RobotEstopResuming {
				t.Errorf("estopState = %s, want Resuming", got.Status.EstopState)
			}
			select {
			case e := <-rec.Events:
				if !strings.HasPrefix(e, "Warning EstopResumeUnanswered") {
					t.Errorf("event %q, want EstopResumeUnanswered", e)
				}
			default:
				t.Errorf("the unanswered claim was not reported")
			}
		})
	}
}
