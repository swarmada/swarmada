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

package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Row I6 (RFC-0001 #safety-how-the-two-interact): the control plane sends a robot a stop holds no
// zone_admission with admit = true and no reservation_granted. It sends either once the robot is
// released (S6), if the hold or the queue it answers still stands.

// ackAll answers every zone_admission and reservation_granted as acknowledged.
func ackAll(d *Dispatcher) *fakeSender {
	return &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		res := &fav1.CommandResult{CommandId: cmd.GetCommandId()}
		switch {
		case cmd.GetZoneAdmission() != nil:
			res.Result = &fav1.CommandResult_ZoneAdmission{ZoneAdmission: &fav1.ZoneAdmissionAck{Acknowledged: true}}
		case cmd.GetReservationGranted() != nil:
			res.Result = &fav1.CommandResult_ReservationGranted{ReservationGranted: &fav1.ReservationGrantedAck{Acknowledged: true}}
		default:
			return nil
		}
		return res
	}}
}

// sentOf counts the zone_admission admit = true and reservation_granted Commands s carried, and
// the fences.
func sentOf(s *fakeSender) (admits, holds, grants, fences int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.sent {
		switch {
		case cmd.GetZoneAdmission() != nil && cmd.GetZoneAdmission().GetAdmit():
			admits++
		case cmd.GetZoneAdmission() != nil:
			holds++
		case cmd.GetReservationGranted() != nil:
			grants++
		case cmd.GetEstop() != nil:
			fences++
		}
	}
	return
}

func setEstop(t *testing.T, c client.Client, state fleetv1.RobotEstopState) {
	t.Helper()
	r := cmdRobot(t, c)
	r.Status.EstopState = state
	if err := c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond for up to two seconds: the release sends from a goroutine of its own.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The red row of the zone-admission rule: a stop reaches a robot held at a zone boundary, then a
// slot frees. No admit = true goes on the wire, through the stop, the clear (Resuming) and until
// the release; after the release it is sent once.
func TestZoneAdmission_AStoppedRobotHeldAtABoundaryIsNotAdmittedUntilItIsReleased(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	s := ackAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	ctx := context.Background()

	if _, err := d.PushZoneAdmission(ctx, cmdNS, cmdRobotID, "aisle-3", false); err != nil {
		t.Fatalf("the hold: %v", err)
	}
	if err := d.SendFence(cmdRobot(t, c), "stop-1", &fav1.Estop{Reason: "hazard"}); err != nil {
		t.Fatal(err)
	}
	setEstop(t, c, fleetv1.RobotEstopStopped)

	// The slot frees while the stop holds the robot.
	if _, err := d.PushZoneAdmission(ctx, cmdNS, cmdRobotID, "aisle-3", true); !errors.Is(err, ErrWithheldByStop) {
		t.Fatalf("admit under a stop: err %v, want ErrWithheldByStop", err)
	}
	// The clear: the stop's fence is no longer in force, and the robot is Resuming.
	d.ClearHeld(cmdNS, cmdRobotID)
	setEstop(t, c, fleetv1.RobotEstopResuming)
	if _, err := d.PushZoneAdmission(ctx, cmdNS, cmdRobotID, "aisle-3", true); !errors.Is(err, ErrWithheldByStop) {
		t.Fatalf("admit while Resuming: err %v, want ErrWithheldByStop", err)
	}
	if admits, _, _, _ := sentOf(s); admits != 0 {
		t.Fatalf("%d zone_admission admit = true on the wire before the release; want none", admits)
	}

	// The release (S6) sends the withheld admit, once.
	setEstop(t, c, fleetv1.RobotEstopNormal)
	d.RobotReleased(cmdNS, cmdRobotID)
	waitFor(t, "the admit after the release", func() bool { a, _, _, _ := sentOf(s); return a == 1 })
	d.RobotReleased(cmdNS, cmdRobotID)
	time.Sleep(50 * time.Millisecond)
	if admits, holds, _, fences := sentOf(s); admits != 1 || holds != 1 || fences != 1 {
		t.Fatalf("after two releases: %d admits, %d holds, %d fences; want 1, 1, 1", admits, holds, fences)
	}
}

// Every state a stop holds the robot in withholds the admit, read from the Robot even when this
// process sent no fence: Stopping, Stopped, Failed, Resuming, the robot's own trigger and a
// recorded zone stop. Normal sends it.
func TestZoneAdmission_TheRobotsStopStateWithholdsTheAdmit(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fleetv1.Robot)
		held bool
	}{
		{"normal", func(*fleetv1.Robot) {}, false},
		{"stopping", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopping }, true},
		{"stopped", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped }, true},
		{"failed", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopFailed }, true},
		{"resuming", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming }, true},
		{"own-trigger", func(r *fleetv1.Robot) {
			r.Annotations = map[string]string{estopscope.TriggerAnnotation: "hazard"}
		}, true},
		{"recorded-zone-stop", func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "aisle-3"}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c := newDispatcher(t, cmdAdapter)
			r := cmdRobot(t, c)
			tc.set(r)
			if err := c.Update(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			s := ackAll(d)
			d.RegisterStream(verifiedIdentity(), s)
			_, err := d.PushZoneAdmission(context.Background(), cmdNS, cmdRobotID, "aisle-3", true)
			admits, _, _, _ := sentOf(s)
			if tc.held && (!errors.Is(err, ErrWithheldByStop) || admits != 0) {
				t.Fatalf("err %v, %d admits; want ErrWithheldByStop and none sent", err, admits)
			}
			if !tc.held && (err != nil || admits != 1) {
				t.Fatalf("err %v, %d admits; want it sent", err, admits)
			}
		})
	}
}

// A hold (admit = false) moves nothing, so a stop never withholds it, and it drops the admit for
// that zone withheld before it: the release then sends nothing for that zone.
func TestZoneAdmission_AHoldIsNeverWithheldAndDropsTheWithheldAdmit(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	s := ackAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	ctx := context.Background()
	if err := d.SendFence(cmdRobot(t, c), "stop-1", &fav1.Estop{Reason: "hazard"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PushZoneAdmission(ctx, cmdNS, cmdRobotID, "aisle-3", true); !errors.Is(err, ErrWithheldByStop) {
		t.Fatalf("admit under a stop: %v", err)
	}
	if _, err := d.PushZoneAdmission(ctx, cmdNS, cmdRobotID, "aisle-3", false); err != nil {
		t.Fatalf("a hold under a stop: %v, want it sent", err)
	}
	if _, holds, _, _ := sentOf(s); holds != 1 {
		t.Fatalf("%d holds on the wire; want 1", holds)
	}
	d.ClearHeld(cmdNS, cmdRobotID)
	d.RobotReleased(cmdNS, cmdRobotID)
	time.Sleep(100 * time.Millisecond)
	if admits, _, _, _ := sentOf(s); admits != 0 {
		t.Fatalf("%d admits after the release; the later hold dropped it, want none", admits)
	}
}

// promotedZone is a FleetZone whose status records robot as an adapter-initiated holder of
// resource (the TDE's promotion, persisted in FleetZone.status).
func promotedZone(resource, robot string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: cmdNS, Name: "dock"},
		Status: fleetv1.FleetZoneStatus{SharedResourceQueues: []fleetv1.SharedResourceQueue{{
			ResourceName: resource, CurrentHolders: []fleetv1.ResourceHolder{{RobotID: robot, ActionID: robot}},
		}}},
	}
}

// A grant to a robot a stop holds is withheld, and sent after the release only while the
// promotion it answers still stands in FleetZone.status.
func TestReservationGrant_WithheldUnderAStopAndSentAfterTheReleaseWhileThePromotionStands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stands bool
	}{{"promotion-stands", true}, {"promotion-gone", false}} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := fleetv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			objs := []client.Object{&fleetv1.Robot{
				ObjectMeta: metav1.ObjectMeta{Namespace: cmdNS, Name: cmdRobotID},
				Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: cmdAdapter}},
			}}
			if tc.stands {
				objs = append(objs, promotedZone("lift", cmdRobotID))
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			d := New(c)
			s := ackAll(d)
			d.RegisterStream(verifiedIdentity(), s)
			if err := d.SendFence(cmdRobot(t, c), "stop-1", &fav1.Estop{Reason: "hazard"}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.PushReservationGranted(context.Background(), cmdNS, cmdRobotID, "lift"); !errors.Is(err, ErrWithheldByStop) {
				t.Fatalf("grant under a stop: err %v, want ErrWithheldByStop", err)
			}
			if _, _, grants, _ := sentOf(s); grants != 0 {
				t.Fatalf("%d grants on the wire under a stop; want none", grants)
			}
			d.ClearHeld(cmdNS, cmdRobotID)
			d.RobotReleased(cmdNS, cmdRobotID)
			if tc.stands {
				waitFor(t, "the grant after the release", func() bool { _, _, g, _ := sentOf(s); return g == 1 })
				return
			}
			time.Sleep(100 * time.Millisecond)
			if _, _, grants, _ := sentOf(s); grants != 0 {
				t.Fatalf("%d grants after the release with no promotion standing; want none", grants)
			}
		})
	}
}

// Run under -race: whatever the interleaving with a stop's fence, an admit = true is on the wire
// ahead of the fence, or it is not sent.
func TestZoneAdmission_AnAdmitIsAheadOfTheFenceOrNotSent(t *testing.T) {
	for i := 0; i < 2000; i++ {
		d, c := newDispatcher(t, cmdAdapter)
		s := ackAll(d)
		d.RegisterStream(verifiedIdentity(), s)
		robot := cmdRobot(t, c)
		var wg sync.WaitGroup
		var admitErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, admitErr = d.PushZoneAdmission(context.Background(), cmdNS, cmdRobotID, "aisle-3", true)
		}()
		go func() {
			defer wg.Done()
			_ = d.SendFence(robot, fmt.Sprintf("stop-%d", i), &fav1.Estop{Reason: "hazard"})
		}()
		wg.Wait()
		s.mu.Lock()
		fenced, admitted := false, false
		for _, cmd := range s.sent {
			switch {
			case cmd.GetEstop() != nil:
				fenced = true
			case cmd.GetZoneAdmission() != nil:
				if fenced {
					s.mu.Unlock()
					t.Fatalf("iteration %d: an admit went on the wire after the fence", i)
				}
				admitted = true
			}
		}
		s.mu.Unlock()
		if !admitted && !errors.Is(admitErr, ErrWithheldByStop) {
			t.Fatalf("iteration %d: the admit was not sent, and the error is %v, not ErrWithheldByStop", i, admitErr)
		}
	}
}

// staleRead returns, on its first Get, the Robot as it was before during ran: what a read that
// races a stop and its clear returns.
type staleRead struct {
	client.Client
	once   sync.Once
	during func()
}

func (s *staleRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := s.Client.Get(ctx, key, obj, opts...)
	s.once.Do(s.during)
	return err
}

// A stop sent and cleared while the gate reads the Robot: the read still says Normal and the
// stop's fence is no longer in force, but a stop was added since the read, so the Robot is read
// again, and it is Resuming. The admit is withheld.
func TestZoneAdmission_AStopAndItsClearDuringTheReadWithholdTheAdmit(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	s := ackAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	d.APIReader = &staleRead{Client: c, during: func() {
		if err := d.SendFence(cmdRobot(t, c), "stop-1", &fav1.Estop{Reason: "hazard"}); err != nil {
			t.Error(err)
		}
		setEstop(t, c, fleetv1.RobotEstopResuming)
		d.ClearHeld(cmdNS, cmdRobotID)
	}}
	if _, err := d.PushZoneAdmission(context.Background(), cmdNS, cmdRobotID, "aisle-3", true); !errors.Is(err, ErrWithheldByStop) {
		t.Fatalf("admit across a stop and its clear: err %v; want ErrWithheldByStop", err)
	}
	if admits, _, _, _ := sentOf(s); admits != 0 {
		t.Fatalf("%d admits sent; want none", admits)
	}
}
