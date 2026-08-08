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

package controlstream

import (
	"context"
	"sync"
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// fakeReserver grants "lift" and denies anything else; release always succeeds and
// reports a promoted waiter (to exercise the async reservation_granted push).
type fakeReserver struct{}

func (fakeReserver) ReserveResource(_ context.Context, _, resourceName, _ string) ResourceDecision {
	if resourceName == "lift" {
		return ResourceDecision{State: ResourceGranted}
	}
	return ResourceDecision{State: ResourceDenied, Message: "unknown resource"}
}

func (fakeReserver) ReleaseResource(_ context.Context, _, _, _ string) ResourceDecision {
	return ResourceDecision{Released: true, PromotedRobotID: "amr-2"}
}

// fakeGrantNotifier records the robots it was asked to notify.
type fakeGrantNotifier struct {
	mu    sync.Mutex
	calls []string // "robotID/resourceName"
}

func (f *fakeGrantNotifier) NotifyReservationGranted(_ context.Context, _, robotID, resourceName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, robotID+"/"+resourceName)
}

func (f *fakeGrantNotifier) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// reserve sends a ResourceRequest(reserve) and returns the correlated response.
func sendReserve(t *testing.T, stream fav1.FleetAdapterService_ControlStreamClient, reqID uint64, resource string) *fav1.ResourceResponse {
	t.Helper()
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ResourceRequest{
		ResourceRequest: &fav1.ResourceRequest{
			RequestId: reqID, RobotId: "amr-1",
			Request: &fav1.ResourceRequest_Reserve{Reserve: &fav1.ReserveResource{ResourceName: resource}},
		},
	}}); err != nil {
		t.Fatalf("send resource request: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv resource response: %v", err)
	}
	rr := msg.GetResourceResponse()
	if rr == nil {
		t.Fatalf("expected a ResourceResponse, got %T", msg.GetPayload())
	}
	return rr
}

// A reserve request is dispatched to the reserver and its verdict is returned in a
// ResourceResponse correlated by request_id (§5.4.5).
func TestControlStream_ResourceReserveGranted(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{Reservations: fakeReserver{}})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}

	resp := sendReserve(t, stream, 7, "lift")
	if resp.GetRequestId() != 7 {
		t.Fatalf("request_id = %d, want 7 (correlation)", resp.GetRequestId())
	}
	if got := resp.GetReserve().GetState(); got != fav1.ReservationState_RESERVATION_STATE_GRANTED {
		t.Fatalf("reserve state = %v, want GRANTED", got)
	}
}

// FAIL-CLOSED: with no reserver wired, a reserve request is DENIED, never granted —
// a shared resource is never handed out when the TDE view is absent.
func TestControlStream_ResourceReserveNilReserverDenied(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{}) // no Reservations
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}

	resp := sendReserve(t, stream, 1, "lift")
	if got := resp.GetReserve().GetState(); got != fav1.ReservationState_RESERVATION_STATE_DENIED {
		t.Fatalf("reserve state = %v, want DENIED (fail closed with no reserver)", got)
	}
}

// A release that promotes a queued waiter pushes an async reservation_granted to
// the promoted robot (§5.4.5). The notifier is invoked synchronously in dispatch,
// so it is recorded by the time the release response is received.
func TestControlStream_ResourceReleasePromotionNotifies(t *testing.T) {
	notifier := &fakeGrantNotifier{}
	client, cleanup := newTestServer(t, &Server{Reservations: fakeReserver{}, GrantNotifier: notifier})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}

	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ResourceRequest{
		ResourceRequest: &fav1.ResourceRequest{
			RequestId: 3, RobotId: "amr-1",
			Request: &fav1.ResourceRequest_Release{Release: &fav1.ReleaseResource{ResourceName: "lift"}},
		},
	}}); err != nil {
		t.Fatalf("send release: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv release response: %v", err)
	}
	if !msg.GetResourceResponse().GetRelease().GetReleased() {
		t.Fatal("release should report released")
	}
	if got := notifier.recorded(); len(got) != 1 || got[0] != "amr-2/lift" {
		t.Fatalf("notifier calls = %v, want [amr-2/lift] (promoted robot pushed the grant)", got)
	}
}
