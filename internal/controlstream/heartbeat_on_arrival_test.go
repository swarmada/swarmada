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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A heartbeat proves the adapter alive when it arrives. The messages read before it may wait on
// the API server (a stop's fan-out holds the Robot write budget, and telemetry is handled on the
// same stream), but the adapter's presence and the per-robot confirming exchange it answers are
// recorded as it is read (RFC-0001 #fleetadapter-status-fields-and-phase-machine: lastHeartbeat is
// "when the control plane last confirmed adapter liveness"; ADR-0054).

// gatedIngestor holds every frame until release is closed, as an ingest waiting on the API server
// would, and records the robots in the order it was given them.
type gatedIngestor struct {
	release chan struct{}
	mu      sync.Mutex
	robots  []string
}

func (g *gatedIngestor) Ingest(ctx context.Context, f telemetry.Frame) error {
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.robots = append(g.robots, f.RobotID)
	return nil
}

func (g *gatedIngestor) ingested() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.robots...)
}

// presenceLog records the presence events in the order they happen.
type presenceLog struct {
	mu     sync.Mutex
	events []string
	beatAt []time.Time
}

func (p *presenceLog) add(e string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}
func (p *presenceLog) AdapterConnected(context.Context, TLSIdentity, Negotiation) { p.add("connected") }
func (p *presenceLog) AdapterHeartbeat(context.Context, TLSIdentity)              { p.add("heartbeat") }
func (p *presenceLog) AdapterHeartbeatAt(_ context.Context, _ TLSIdentity, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "heartbeat")
	p.beatAt = append(p.beatAt, at)
}
func (p *presenceLog) AdapterDisconnected(context.Context, TLSIdentity) { p.add("disconnected") }
func (p *presenceLog) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}
func (p *presenceLog) count(e string) int {
	n := 0
	for _, x := range p.snapshot() {
		if x == e {
			n++
		}
	}
	return n
}

// routedBeats records the per-robot heartbeats routed to the confirming exchange.
type routedBeats struct {
	mu     sync.Mutex
	robots []string
}

func (b *routedBeats) RegisterStream(TLSIdentity, CommandSender) func()     { return func() {} }
func (b *routedBeats) RouteResult(CommandSender, *fav1.CommandResult) error { return nil }
func (b *routedBeats) RouteHeartbeat(_ string, robotID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.robots = append(b.robots, robotID)
}
func (b *routedBeats) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.robots...)
}

func heartbeatMsg(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{Heartbeat: &fav1.HeartbeatResponse{RobotId: robotID}}}
}

// waitFor polls cond for up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// Telemetry read before a heartbeat waits on the API server; the heartbeat is still recorded at
// once: the adapter's presence, and the confirming exchange of the robot it names. Once the
// telemetry is let through, every frame is handled, in the order it was sent, before the stream
// closes.
func TestControlStream_AHeartbeatIsRecordedAheadOfAWaitingBacklog(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)

	ing := &gatedIngestor{release: make(chan struct{})}
	pres := &presenceLog{}
	beats := &routedBeats{}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: ing, Presence: pres, Commands: beats,
		Authorizer: &recordAuthorizer{failUnverified: true}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if !waitFor(5*time.Second, func() bool { return pres.count("connected") == 1 }) {
		t.Fatal("the stream never connected")
	}

	sent := []string{"robot-1", "robot-2", "robot-3", "robot-4", "robot-5"}
	for _, r := range sent {
		if err := stream.Send(telemetryMsg(r)); err != nil {
			t.Fatalf("send telemetry: %v", err)
		}
	}
	sentAt := time.Now()
	if err := stream.Send(heartbeatMsg("")); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	if err := stream.Send(heartbeatMsg("robot-3")); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	if !waitFor(time.Second, func() bool { return pres.count("heartbeat") >= 1 }) {
		t.Fatalf("no presence written within 1 s of the heartbeat while telemetry waited ahead of it: %v",
			pres.snapshot())
	}
	if !waitFor(time.Second, func() bool { return len(beats.snapshot()) == 1 }) {
		t.Fatalf("the robot's heartbeat did not answer its confirming exchange while telemetry waited: %v",
			beats.snapshot())
	}
	if got := ing.ingested(); len(got) != 0 {
		t.Fatalf("telemetry handled before it was let through: %v", got)
	}
	pres.mu.Lock()
	at := pres.beatAt[0]
	pres.mu.Unlock()
	if at.Before(sentAt.Add(-time.Second)) || at.After(time.Now()) {
		t.Errorf("presence recorded at %v; want the time the heartbeat was read, after %v", at, sentAt)
	}

	close(ing.release)
	_ = stream.CloseSend()
	if !waitFor(5*time.Second, func() bool { return len(ing.ingested()) == len(sent) }) {
		t.Fatalf("a clean close dropped queued telemetry: handled %v of %v", ing.ingested(), sent)
	}
	for i, r := range ing.ingested() {
		if r != sent[i] {
			t.Fatalf("telemetry handled out of order: %v, sent %v", ing.ingested(), sent)
		}
	}
}

// No presence is written after the stream's disconnect: the presence goroutine is joined before
// the disconnect is reported. Heartbeats sent right before the close race it.
func TestControlStream_NoPresenceAfterTheDisconnect(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	for i := 0; i < 20; i++ {
		pres := &presenceLog{}
		addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}, Presence: pres,
			Authorizer: &recordAuthorizer{failUnverified: true}}, mtlsServerConfig(server, ca.pool))
		stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
		for b := 0; b < 5; b++ {
			_ = stream.Send(heartbeatMsg(""))
		}
		_ = stream.CloseSend()
		if !waitFor(5*time.Second, func() bool { return pres.count("disconnected") == 1 }) {
			stop()
			t.Fatalf("iteration %d: the stream never reported its disconnect: %v", i, pres.snapshot())
		}
		time.Sleep(20 * time.Millisecond) // a late write would land now
		ev := pres.snapshot()
		stop()
		if ev[len(ev)-1] != "disconnected" {
			t.Fatalf("iteration %d: presence written after the disconnect: %v", i, ev)
		}
	}
}

// blockingPresence holds every presence write until release is closed, and records the events in
// order: a presence write still in flight when the stream closes must end before the disconnect.
type blockingPresence struct {
	presenceLog
	release  chan struct{}
	entered  chan struct{}
	enterOne sync.Once
}

func (p *blockingPresence) AdapterHeartbeatAt(ctx context.Context, id TLSIdentity, at time.Time) {
	p.enterOne.Do(func() { close(p.entered) })
	<-p.release
	p.presenceLog.AdapterHeartbeatAt(ctx, id, at)
}

// The stream's disconnect is reported only once the presence write in flight has returned.
func TestControlStream_TheDisconnectWaitsForThePresenceWriteInFlight(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	pres := &blockingPresence{release: make(chan struct{}), entered: make(chan struct{})}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}, Presence: pres,
		Authorizer: &recordAuthorizer{failUnverified: true}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if err := stream.Send(heartbeatMsg("")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pres.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the presence write never started")
	}
	_ = stream.CloseSend()
	time.Sleep(300 * time.Millisecond) // the stream has ended; its disconnect must still wait
	if n := pres.count("disconnected"); n != 0 {
		t.Fatalf("the disconnect was reported while a presence write was still in flight: %v", pres.snapshot())
	}
	close(pres.release)
	if !waitFor(5*time.Second, func() bool { return pres.count("disconnected") == 1 }) {
		t.Fatalf("no disconnect after the presence write returned: %v", pres.snapshot())
	}
	if ev := pres.snapshot(); ev[len(ev)-1] != "disconnected" || pres.count("heartbeat") != 1 {
		t.Fatalf("events %v; want the heartbeat written, then the disconnect", ev)
	}
}

// A loop held on one message past the presence limit is wedged: heartbeats take their turn in its
// queue again, and are recorded, at the time they were read, only once the loop reaches them.
func TestControlStream_AWedgedLoopTakesTheHeartbeatsBack(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	ing := &gatedIngestor{release: make(chan struct{})}
	pres := &presenceLog{}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: ing, Presence: pres, presenceWedge: 300 * time.Millisecond,
		Authorizer: &recordAuthorizer{failUnverified: true}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if !waitFor(5*time.Second, func() bool { return pres.count("connected") == 1 }) {
		t.Fatal("the stream never connected")
	}
	if err := stream.Send(telemetryMsg("robot-1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond) // the loop is held on that frame past the limit
	sentAt := time.Now()
	if err := stream.Send(heartbeatMsg("")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := pres.count("heartbeat"); n != 0 {
		t.Fatalf("a heartbeat skipped a loop wedged past the limit: %v", pres.snapshot())
	}
	close(ing.release)
	if !waitFor(5*time.Second, func() bool { return pres.count("heartbeat") == 1 }) {
		t.Fatalf("the heartbeat was never recorded once the loop reached it: %v", pres.snapshot())
	}
	pres.mu.Lock()
	at := pres.beatAt[0]
	pres.mu.Unlock()
	if at.Before(sentAt.Add(-time.Second)) || at.After(sentAt.Add(400*time.Millisecond)) {
		t.Errorf("recorded at %v; want the time it was read (sent at %v), not the time it was handled", at, sentAt)
	}
	_ = stream.CloseSend()
}

// denyingAuthorizer refuses one robot to every adapter.
type denyingAuthorizer struct{ robot string }

func (a denyingAuthorizer) AuthorizeRobot(_ context.Context, _ TLSIdentity, robotID string) error {
	if robotID == a.robot {
		return errors.New("robot is bound to another adapter")
	}
	return nil
}
func (a denyingAuthorizer) AuthorizeAnnounce(context.Context, TLSIdentity, string) error { return nil }

// A heartbeat naming a robot this adapter may not act for records nothing: no presence, and no
// answer to that robot's confirming exchange.
func TestControlStream_ADeniedRobotsHeartbeatRecordsNothing(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	pres := &presenceLog{}
	beats := &routedBeats{}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}, Presence: pres, Commands: beats,
		Authorizer: denyingAuthorizer{robot: "robot-x"}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if err := stream.Send(heartbeatMsg("robot-x")); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(heartbeatMsg("")); err != nil { // a marker: handled after the one above
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return pres.count("heartbeat") >= 1 }) {
		t.Fatal("the adapter-scoped heartbeat was never recorded")
	}
	time.Sleep(200 * time.Millisecond)
	if got := beats.snapshot(); len(got) != 0 {
		t.Errorf("a denied robot's heartbeat answered its confirming exchange: %v", got)
	}
	if n := pres.count("heartbeat"); n != 1 {
		t.Errorf("%d presence writes; want only the adapter-scoped heartbeat's", n)
	}
	_ = stream.CloseSend()
}
