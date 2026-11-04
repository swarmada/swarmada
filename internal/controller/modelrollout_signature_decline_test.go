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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
)

// A SIGNATURE REFUSAL IS A TERMINAL OUTCOME FOR THAT ROBOT, NOT A DELIVERY PROBLEM.
//
// The adapter refused the artifact because it could not verify it (RFC-0001
// #fleet-adapter-protocol-fleet-adapter-compliance-checklist). Pushing the same artifact
// again cannot change that answer. So the robot fails in this rollout, with the reason,
// pauseOnError halts the rollout, and the refusal is recorded once: one push, one
// MODEL_SIGNATURE_FAILED entry and one Warning Event, however many reconciles follow.

// countingPusher declines every push with a fixed message and counts the pushes.
type countingPusher struct {
	message string
	pushes  int
}

func (p *countingPusher) PushModelUpdate(_ context.Context, _, _ string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.pushes++
	return command.ModelUpdateOutcome{Acknowledged: false, Message: p.message}, nil
}

const sigDecline = "SignatureVerificationFailed: trust root mismatch"

func sigRollout(t *testing.T, r *ModelRolloutReconciler) *fleetv1.ModelRollout {
	t.Helper()
	ro := &fleetv1.ModelRollout{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, ro); err != nil {
		t.Fatal(err)
	}
	return ro
}

func countEvents(rec *record.FakeRecorder, reason string) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, " "+reason+" ") {
				n++
			}
		default:
			return n
		}
	}
}

func TestModelRollout_SignatureDeclineFailsTheRobotAndPauses(t *testing.T) {
	aud := &recordingAudit{}
	events := record.NewFakeRecorder(100)
	pusher := &countingPusher{message: sigDecline}
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher, r.Audit, r.Recorder = pusher, aud, events

	for i := 0; i < 5; i++ {
		reconcileRollout(t, r)
	}

	ro := sigRollout(t, r)
	if ro.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("pauseOnError is set and a robot failed: want phase Paused, got %q", ro.Status.Phase)
	}
	if ro.Status.RobotsFailed != 1 || len(ro.Status.FailedRobots) != 1 || ro.Status.FailedRobots[0].RobotName != "r1" {
		t.Fatalf("the refused robot must be recorded failed: robotsFailed=%d failedRobots=%+v",
			ro.Status.RobotsFailed, ro.Status.FailedRobots)
	}
	if reason := ro.Status.FailedRobots[0].Reason; !strings.Contains(reason, "SignatureVerificationFailed") {
		t.Errorf("the failure reason must carry the adapter's refusal, got %q", reason)
	}
	if ro.Status.RobotsPending != 0 {
		t.Errorf("a failed robot is not pending: robotsPending=%d", ro.Status.RobotsPending)
	}
	if pusher.pushes != 1 {
		t.Errorf("an artifact refused on its signature must not be pushed again: %d pushes", pusher.pushes)
	}
	if n := len(aud.ofType(audit.EventModelSignatureFailed)); n != 1 {
		t.Errorf("want one MODEL_SIGNATURE_FAILED for one refusal, got %d", n)
	}
	if n := len(aud.ofType(audit.EventModelRolloutPaused)); n != 1 {
		t.Errorf("want one MODEL_ROLLOUT_PAUSED on the pause edge, got %d", n)
	}
	if n := countEvents(events, "ModelRolloutPushDeclined"); n != 1 {
		t.Errorf("want one ModelRolloutPushDeclined Event for one refusal, got %d", n)
	}
	// The robot is untouched: it never installed the model, so nothing marks it.
	if rob := rolloutRobot(t, c, "r1"); len(rob.Status.InstalledModels) != 0 {
		t.Errorf("a refused artifact must leave the robot's models as they were: %+v", rob.Status.InstalledModels)
	}
}

func TestModelRollout_SignatureDeclineWithoutPauseOnErrorFailsTheRollout(t *testing.T) {
	ro := pickerRollout()
	ro.Spec.Strategy.RollingUpdate.PauseOnError = apiBool(false)
	pusher := &countingPusher{message: sigDecline}
	r, _ := newRolloutReconciler(t, ro, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher

	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}
	if got := sigRollout(t, r).Status.Phase; got != fleetv1.RolloutPhaseFailed {
		t.Errorf("every robot has failed or settled and nothing pauses: want Failed, got %q", got)
	}
	if pusher.pushes != 1 {
		t.Errorf("want one push, got %d", pusher.pushes)
	}
}

// Under pauseOnError the first refusal stops the batch: no further robot is pushed the
// artifact in the same pass.
func TestModelRollout_SignatureDeclineStopsTheBatch(t *testing.T) {
	ro := pickerRollout()
	ro.Spec.Strategy.RollingUpdate.MaxUnavailable = "2"
	pusher := &countingPusher{message: sigDecline}
	r, _ := newRolloutReconciler(t, ro,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90), targetRobot("r2", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher

	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}
	if pusher.pushes != 1 {
		t.Errorf("pauseOnError must stop at the first refusal: %d pushes", pusher.pushes)
	}
	if st := sigRollout(t, r).Status; st.Phase != fleetv1.RolloutPhasePaused || st.RobotsFailed != 1 {
		t.Errorf("want Paused with one failed robot, got phase=%q robotsFailed=%d", st.Phase, st.RobotsFailed)
	}
}

// The operator's exit from the halt is the rollout-resume verb (ADR-0041): it excludes the
// refused robot, and the rollout settles instead of pushing the refused artifact again.
func TestModelRollout_ResumePastASignatureDecline(t *testing.T) {
	pusher := &countingPusher{message: sigDecline}
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher
	reconcileRollout(t, r)
	reconcileRollout(t, r)

	ro := sigRollout(t, r)
	if ro.Annotations == nil {
		ro.Annotations = map[string]string{}
	}
	ro.Annotations[rolloutResumeAnnotation] = "operator checked the trust root"
	if err := c.Update(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}
	st := sigRollout(t, r).Status
	if st.Phase != fleetv1.RolloutPhaseSucceeded || len(st.ExcludedRobots) != 1 || st.RobotsFailed != 0 {
		t.Errorf("resume must exclude the refused robot and settle: phase=%q excluded=%v robotsFailed=%d",
			st.Phase, st.ExcludedRobots, st.RobotsFailed)
	}
	if pusher.pushes != 1 {
		t.Errorf("resume must not push the refused artifact again: %d pushes", pusher.pushes)
	}
}

// A decline for any other reason is a delivery problem and is still retried.
func TestModelRollout_OrdinaryDeclineIsRetried(t *testing.T) {
	pusher := &countingPusher{message: "robot busy; retry later"}
	r, _ := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	r.Pusher = pusher
	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}
	if pusher.pushes != 3 {
		t.Errorf("an ordinary decline is retried on each pass: %d pushes", pusher.pushes)
	}
	if st := sigRollout(t, r).Status; st.RobotsFailed != 0 {
		t.Errorf("an ordinary decline does not fail the robot: robotsFailed=%d", st.RobotsFailed)
	}
}
