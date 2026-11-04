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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// refusingDispatcher is newDispatcher whose client refuses every robots/status write that
// sets estopState to refuse.
func refusingDispatcher(t *testing.T, refuse fleetv1.RobotEstopState, robot *fleetv1.Robot) (*Dispatcher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				if r, ok := obj.(*fleetv1.Robot); ok && sub == "status" && r.Status.EstopState == refuse {
					return errors.New("injected: estopState " + string(refuse) + " refused")
				}
				return cl.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).Build()
	d := New(atomicFake(c), record.NewFakeRecorder(10))
	d.Telemetry = &testFrames{}
	d.DeriveCapabilities = testDerivation
	d.deliveryTimeout = 60 * time.Millisecond
	d.confirmTimeout = 60 * time.Millisecond
	return d, c
}

func plainRobot() *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: "amr-1"}},
		Spec: fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "acme", Version: "1.0.0"}},
	}
}

// A stop's outcome whose estopState write is refused is reported as unwritten, at every
// outcome, so the caller does not record the stop as issued.
func TestTriggerEstop_AnUnwrittenOutcomeIsReported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse fleetv1.RobotEstopState
		reply  func(id string) []*fav1.EstopAck
		stream bool
		want   fleetv1.RobotEstopState
	}{
		{name: "confirmed STOPPED", refuse: fleetv1.RobotEstopStopped, stream: true, want: fleetv1.RobotEstopStopped,
			reply: func(id string) []*fav1.EstopAck {
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
			}},
		{name: "STOPPING then STOPPED", refuse: fleetv1.RobotEstopStopped, stream: true, want: fleetv1.RobotEstopStopped,
			reply: func(id string) []*fav1.EstopAck {
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING), ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
			}},
		{name: "adapter FAILED", refuse: fleetv1.RobotEstopFailed, stream: true, want: fleetv1.RobotEstopFailed,
			reply: func(id string) []*fav1.EstopAck {
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_FAILED)}
			}},
		{name: "no SafetyStream", refuse: fleetv1.RobotEstopFailed, want: fleetv1.RobotEstopFailed},
		{name: "dropped estop", refuse: fleetv1.RobotEstopFailed, stream: true, want: fleetv1.RobotEstopFailed},
		{name: "STOPPING, no confirmation", refuse: fleetv1.RobotEstopFailed, stream: true, want: fleetv1.RobotEstopFailed,
			reply: func(id string) []*fav1.EstopAck {
				return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c := refusingDispatcher(t, tc.refuse, plainRobot())
			if tc.stream {
				d.RegisterStream(identity("acme"), &scriptedSender{d: d, reply: tc.reply})
			}
			res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot, Carrier{})
			if !errors.Is(err, ErrStateUnwritten) {
				t.Fatalf("err = %v, want ErrStateUnwritten", err)
			}
			if res.State != tc.want {
				t.Fatalf("Result.State = %q, want %q (what the adapter answered)", res.State, tc.want)
			}
			if got := estopState(t, c); got == tc.want {
				t.Fatalf("estopState = %q though its write was refused", got)
			}
		})
	}
}

// A refused intermediate Stopping write does not end the stop: the outcome is written.
func TestTriggerEstop_ARefusedStoppingWriteDoesNotEndTheStop(t *testing.T) {
	d, c := refusingDispatcher(t, fleetv1.RobotEstopStopping, plainRobot())
	d.RegisterStream(identity("acme"), &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPING), ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}})
	if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "test", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
		t.Fatalf("TriggerEstop: %v", err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %q, want Stopped", got)
	}
}

// A robot that reads Normal but records a zone's or the namespace's stop (its stop state
// was never written) is cleared to Resuming, so it too waits for a resume.
func TestClearEstop_ARecordedStopOnANormalRobotClearsToResuming(t *testing.T) {
	robot := plainRobot()
	robot.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}
	d, c := refusingDispatcher(t, "never", robot)
	// The zone's clear ends its record (the controller's clearEnds).
	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "operator",
		Ends{Hold: func(h fleetv1.EstopHold) bool { return h.Name == "z" }})
	if err != nil || state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %q, %v; want Resuming", state, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %q, want Resuming", got)
	}
}

// A refused Resuming write is an error, not a clear the caller counts as applied.
func TestClearEstop_ARefusedWriteIsAnError(t *testing.T) {
	robot := plainRobot()
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	d, c := refusingDispatcher(t, fleetv1.RobotEstopResuming, robot)
	if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "operator", Ends{}); err == nil {
		t.Fatal("ClearEstop returned no error though its Resuming write was refused")
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %q, want Stopped", got)
	}
}

// The release is refused while a recorded stop holds the robot.
func TestConfirmResume_RefusedWhileAStopIsRecorded(t *testing.T) {
	robot := plainRobot()
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	robot.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}
	d, c := refusingDispatcher(t, "never", robot)
	state, why, err := d.ConfirmResume(context.Background(), ns, "amr-1", "operator")
	if err != nil || state != fleetv1.RobotEstopResuming || !strings.Contains(why, "zone z") {
		t.Fatalf("ConfirmResume = %q, %q, %v; want Resuming, held by zone z", state, why, err)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %q, want Resuming", got)
	}
}
