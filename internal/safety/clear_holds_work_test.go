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

package safety

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// The clear holds the work (RFC-0001 #safety-action-table, The lease): after the clear nothing about
// the work the robot records resumes automatically, lease renewals included, until the robot's own
// report or an operator's decision settles it; the operator release (S6) releases the robot, not its
// work. The clear (S5) records that hold on the work itself, the condition RenewalHeld, so it does not
// depend on a FleetAction reconcile reading the robot Resuming before the release. The release records
// it on any work the clear's process did not reach before it writes Normal, and refuses while it cannot.

// workClient refuses FleetAction status writes while refuse is set, and logs, in order, each
// FleetAction status write ("hold:<name>") and each Robot status write ("robot:<estopState>").
type workClient struct {
	client.Client
	refuse *atomic.Bool
	mu     *sync.Mutex
	log    *[]string
	// robotLists counts the Robot lists that have returned, as hookClient's does: a SafetyStream's
	// opening pass reads the adapter's robots once.
	robotLists int
}

func (w *workClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := w.Client.List(ctx, list, opts...)
	if _, ok := list.(*fleetv1.RobotList); ok {
		w.mu.Lock()
		w.robotLists++
		w.mu.Unlock()
	}
	return err
}

func (w *workClient) Status() client.SubResourceWriter {
	return &workStatus{SubResourceWriter: w.Client.Status(), w: w}
}

type workStatus struct {
	client.SubResourceWriter
	w *workClient
}

func (s *workStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	if a, ok := obj.(*fleetv1.FleetAction); ok {
		if s.w.refuse.Load() {
			return errors.New("refused by the test: the API server is unreachable")
		}
		if err := s.SubResourceWriter.Patch(ctx, obj, p, opts...); err != nil {
			return err
		}
		s.w.mu.Lock()
		*s.w.log = append(*s.w.log, "hold:"+a.Name)
		s.w.mu.Unlock()
		return nil
	}
	err := s.SubResourceWriter.Patch(ctx, obj, p, opts...)
	if r, ok := obj.(*fleetv1.Robot); ok && err == nil {
		s.w.mu.Lock()
		*s.w.log = append(*s.w.log, "robot:"+string(r.Status.EstopState))
		s.w.mu.Unlock()
	}
	return err
}

func heldAction(name, robot string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: phase, AssignedRobot: robot, AssignmentGeneration: 3},
	}
}

// workRig is newDispatcher's dispatcher on a client that also serves FleetAction status, with the
// robot amr-1 recording t-run, the actions given, and a confirmed robot-scope stop recorded.
func workRig(t *testing.T, actions ...*fleetv1.FleetAction) (*Dispatcher, *workClient, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{&fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: "amr-1"}},
		Spec:   fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
		Status: fleetv1.RobotStatus{AssignedAction: "t-run"},
	}}
	for _, a := range actions {
		objs = append(objs, a)
	}
	c := atomicFake(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.FleetAction{}).Build())
	w := &workClient{Client: c, refuse: &atomic.Bool{}, mu: &sync.Mutex{}, log: &[]string{}}
	rec := record.NewFakeRecorder(50)
	d := New(w, rec)
	d.Telemetry = &testFrames{}
	d.DeriveCapabilities = testDerivation
	d.deliveryTimeout = 60 * time.Millisecond
	d.confirmTimeout = 60 * time.Millisecond
	d.RegisterStream(identity("acme"), stoppedSender(d))
	// The stream's opening pass reads the robot in a goroutine of its own. Read after the stop below
	// is written and before its record lands, the robot would be a stopped robot that records no stop,
	// and the pass would send it a stop of its own (resendHeld), as in clearRigAt. The stop waits until
	// the pass has read.
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		w.mu.Lock()
		n := w.robotLists
		w.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("setup: the stream's opening pass never read the robot")
		}
	}
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
		t.Fatal(err)
	}
	waitRecorded(t, c, 1)
	w.mu.Lock()
	*w.log = nil
	w.mu.Unlock()
	return d, w, rec
}

func holdOf(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()
	a := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, a); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(a.Status.Conditions, ConditionRenewalHeld)
}

// The clear holds every action bound to the robot whose lease a clear holds, with the reason of its
// phase, and no other action.
func TestClear_HoldsTheWorkOfTheRobotItClears(t *testing.T) {
	d, w, _ := workRig(t,
		heldAction("t-run", "amr-1", fleetv1.ActionPhaseInProgress),
		heldAction("t-paused", "amr-1", fleetv1.ActionPhasePaused),
		heldAction("t-revoking", "amr-1", fleetv1.ActionPhaseRevoking),
		heldAction("t-other", "amr-2", fleetv1.ActionPhaseInProgress),
		heldAction("t-done", "amr-1", fleetv1.ActionPhaseSucceeded))
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	for name, reason := range map[string]string{
		"t-run": ReasonStopClearedBeforePause, "t-revoking": ReasonStopClearedBeforePause,
		"t-paused": ReasonStopClearedWhilePaused,
	} {
		if c := holdOf(t, w, name); c == nil || c.Reason != reason {
			t.Errorf("%s: condition %+v; want %s/%s", name, c, ConditionRenewalHeld, reason)
		}
	}
	for _, name := range []string{"t-other", "t-done"} {
		if c := holdOf(t, w, name); c != nil {
			t.Errorf("%s: condition %+v; want none (another robot's work, or settled work)", name, c)
		}
	}
}

// The clear's process did not reach the work (a change of leader between the clear's write and the
// hold): the release holds it before it writes Normal.
func TestRelease_HoldsTheWorkTheClearDidNotReachBeforeNormal(t *testing.T) {
	d, w, _ := workRig(t, heldAction("t-run", "amr-1", fleetv1.ActionPhaseInProgress))
	w.refuse.Store(true)
	if st, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v", st, err)
	}
	if c := holdOf(t, w, "t-run"); c != nil {
		t.Fatalf("setup: the clear held the work although its writes were refused: %+v", c)
	}
	w.refuse.Store(false)
	w.mu.Lock()
	*w.log = nil
	w.mu.Unlock()
	projected(t, d, w, time.Now().Add(time.Minute), fleetv1.RobotPhaseIdle)
	if st, _, err := d.ConfirmResume(context.Background(), ns, "amr-1", "alice"); err != nil || st != fleetv1.RobotEstopNormal {
		t.Fatalf("ConfirmResume = %s, %v; want Normal", st, err)
	}
	if c := holdOf(t, w, "t-run"); c == nil || c.Reason != ReasonStopClearedBeforePause {
		t.Fatalf("after the release: condition %+v; want %s/%s", c, ConditionRenewalHeld, ReasonStopClearedBeforePause)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	hold, normal := -1, -1
	for i, e := range *w.log {
		switch e {
		case "hold:t-run":
			hold = i
		case "robot:" + string(fleetv1.RobotEstopNormal):
			normal = i
		}
	}
	if hold < 0 || normal < 0 || hold > normal {
		t.Fatalf("writes %v; want the hold on t-run before the Normal write", *w.log)
	}
}

// The release does not write Normal while the work cannot be held.
func TestRelease_RefusesWhileTheWorkCannotBeHeld(t *testing.T) {
	d, w, _ := workRig(t, heldAction("t-run", "amr-1", fleetv1.ActionPhaseInProgress))
	w.refuse.Store(true)
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "alice", Ends{}); err != nil {
		t.Fatal(err)
	}
	projected(t, d, w, time.Now().Add(time.Minute), fleetv1.RobotPhaseIdle)
	st, _, err := d.ConfirmResume(context.Background(), ns, "amr-1", "alice")
	if st == fleetv1.RobotEstopNormal || err == nil || estopState(t, w) != fleetv1.RobotEstopResuming {
		t.Fatalf("ConfirmResume = %s, %v (stored %s); want an error and the robot left Resuming",
			st, err, estopState(t, w))
	}
}
