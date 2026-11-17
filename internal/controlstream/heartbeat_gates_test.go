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
	"sync/atomic"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/telemetry"
)

// What a heartbeat may record, and when: only the stream that holds the adapter's identity, on
// the leader, speaks for the adapter; a loop is wedged when it is held on one message, not when it
// runs behind; and a heartbeat that waited its turn answers a confirming exchange only while it is
// recent.

type notLeading struct{}

func (notLeading) Leading() (<-chan struct{}, bool) { return nil, false }

func closedChan() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

var gateID = TLSIdentity{AdapterName: "acme", Namespace: "ns", Verified: true}

// A superseded stream, or a process that no longer leads, writes no presence, whichever path the
// heartbeat took.
func TestRecordPresence_NoneForASupersededStreamOrAFollower(t *testing.T) {
	ctx := context.Background()
	pres := &presenceLog{}
	(&Server{Presence: pres}).recordPresence(withSuperseded(ctx, closedChan()), gateID, time.Now())
	(&Server{Presence: pres, Leadership: notLeading{}}).recordPresence(ctx, gateID, time.Now())
	if n := pres.count("heartbeat"); n != 0 {
		t.Fatalf("%d presence writes from a superseded stream or a follower; want none", n)
	}
	(&Server{Presence: pres}).recordPresence(withSuperseded(ctx, make(chan struct{})), gateID, time.Now())
	if n := pres.count("heartbeat"); n != 1 {
		t.Fatalf("%d presence writes from the stream holding the identity; want 1", n)
	}
}

// On arrival, a heartbeat of a superseded stream, or on a process that no longer leads, records
// nothing: no presence and no answer to a confirming exchange. It is left to the loop.
func TestHeartbeatOnArrival_ASupersededStreamOrAFollowerRecordsNothing(t *testing.T) {
	ctx := context.Background()
	open := make(chan struct{})
	for _, c := range []struct {
		name string
		lead Leadership
		ctx  context.Context
	}{
		{"superseded", nil, withSuperseded(ctx, closedChan())},
		{"not leading", notLeading{}, ctx},
	} {
		beats := &routedBeats{}
		pump := &presencePump{notify: make(chan struct{}, 1)}
		var handling atomic.Int64
		s := &Server{Commands: beats, Leadership: c.lead}
		if s.heartbeatOnArrival(c.ctx, gateID, heartbeatMsg("robot-3"), pump, &handling, open) {
			t.Errorf("%s: the heartbeat was recorded on arrival", c.name)
		}
		if got := beats.snapshot(); len(got) != 0 {
			t.Errorf("%s: the heartbeat answered a confirming exchange: %v", c.name, got)
		}
	}
	beats := &routedBeats{}
	pump := &presencePump{notify: make(chan struct{}, 1)}
	var handling atomic.Int64
	if !(&Server{Commands: beats}).heartbeatOnArrival(withSuperseded(ctx, make(chan struct{})), gateID,
		heartbeatMsg("robot-3"), pump, &handling, open) {
		t.Fatal("the stream holding the identity did not record its heartbeat on arrival")
	}
	if got := beats.snapshot(); len(got) != 1 {
		t.Fatalf("routed %v; want robot-3's confirming exchange answered", got)
	}
}

// A heartbeat handled in the loop's order answers a confirming exchange only while it is recent,
// and never for a superseded stream.
func TestDispatch_AHeartbeatThatWaitedAnswersNoConfirmingExchange(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		ctx    context.Context
		answer bool
	}{
		{"read 1 s ago", withReceivedAt(ctx, time.Now().Add(-time.Second)), true},
		{"read 11 s ago", withReceivedAt(ctx, time.Now().Add(-11*time.Second)), false},
		{"superseded", withSuperseded(withReceivedAt(ctx, time.Now()), closedChan()), false},
	} {
		beats := &routedBeats{}
		s := &Server{Commands: beats}
		if err := s.dispatch(c.ctx, nil, AdapterIdentity{}, gateID, heartbeatMsg("robot-3")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(beats.snapshot()) == 1; got != c.answer {
			t.Errorf("%s: answered = %v, want %v", c.name, got, c.answer)
		}
	}
}

// slowIngestor takes each frame a while, as an ingest on a busy API server does, and counts them.
type slowIngestor struct {
	each time.Duration
	mu   sync.Mutex
	n    int
}

func (g *slowIngestor) Ingest(ctx context.Context, _ telemetry.Frame) error {
	select {
	case <-time.After(g.each):
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	return nil
}

func (g *slowIngestor) handled() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// A loop running behind, each message handled in turn, is not wedged, however long ago its
// current message was read: a heartbeat is still recorded on arrival, ahead of the rest.
func TestControlStream_ABacklogHandledInTurnIsNotAWedge(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	ing := &slowIngestor{each: 100 * time.Millisecond}
	pres := &presenceLog{}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: ing, Presence: pres, presenceWedge: 300 * time.Millisecond,
		Authorizer: &recordAuthorizer{failUnverified: true}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if !waitFor(5*time.Second, func() bool { return pres.count("connected") == 1 }) {
		t.Fatal("the stream never connected")
	}
	const frames = 15
	for i := 0; i < frames; i++ {
		if err := stream.Send(telemetryMsg("robot-1")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(700 * time.Millisecond) // the frame being handled was read 700 ms ago, past the limit
	if err := stream.Send(heartbeatMsg("")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return pres.count("heartbeat") == 1 }) {
		t.Fatal("the heartbeat was never recorded")
	}
	if n := ing.handled(); n >= frames {
		t.Fatalf("the heartbeat was recorded only after the whole backlog (%d frames) was handled; "+
			"a loop running behind is not wedged", n)
	}
	_ = stream.CloseSend()
}
